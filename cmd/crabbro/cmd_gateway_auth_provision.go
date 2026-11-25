package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/provider"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/siwc"
	"gopkg.in/yaml.v3"
)

var gatewayPlanID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func gatewayAuthProvision(ctx context.Context, cfg *clientConfig, target, planID string, method provider.AuthKind, fromStdin bool, input io.Reader, out io.Writer, login func(siwc.Store, gatewaySIWCStatusEntry) error) (err error) {
	if !gatewayPlanID.MatchString(target) || (planID != "" && !gatewayPlanID.MatchString(planID)) {
		return errors.New("invalid gateway plan or provider ID")
	}
	loginExisting := func(refs gatewaySIWCStatusEntry) error {
		if err := login(gatewayAuthStore(), refs); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, gatewayAuthSessionSaved); err != nil {
			return errors.New("print gateway authentication status failed")
		}
		return nil
	}
	// A local plan takes precedence over a provider with the same ID.
	local, exists, err := readGatewayAuthPlan(target)
	if err != nil {
		return err
	}
	if exists {
		if planID != "" && planID != target {
			return errors.New("--plan cannot rename an existing plan")
		}
		return loginGatewayExisting(local, method, fromStdin, input, out, loginExisting)
	}
	p, isProvider := provider.Lookup(target)
	if planID == "" {
		refs, remoteErr := gatewayAuthPlan(cfg, target)
		if remoteErr == nil {
			if method != "" && method != provider.OAuthSIWC || fromStdin {
				return errors.New("existing plan uses oauth-siwc")
			}
			return loginExisting(refs)
		}
		if !isProvider || !errors.Is(remoteErr, errGatewayPlanNotFound) {
			return remoteErr
		}
		planID = target
	} else if !isProvider {
		return errors.New("provider not found")
	}
	if !isProvider {
		return errors.New("provider not found")
	}
	if method == "" {
		if len(p.SupportedAuth) != 1 {
			choices := make([]string, 0, len(p.SupportedAuth))
			for _, m := range p.SupportedAuth {
				choices = append(choices, string(m.Kind))
			}
			return fmt.Errorf("choose an authentication method with --method: %s", strings.Join(choices, ", "))
		}
		method = p.SupportedAuth[0].Kind
	}
	supported := false
	for _, m := range p.SupportedAuth {
		if m.Kind == method {
			supported = true
		}
	}
	if !supported {
		return errors.New("authentication method is not supported by provider")
	}
	if method != provider.APIKey && method != provider.OAuthSIWC {
		return errors.New("this authentication method cannot be provisioned by login")
	}
	if fromStdin && method != provider.APIKey {
		return errors.New("--stdin requires api-key authentication")
	}
	if existing, found, err := readGatewayAuthPlan(planID); err != nil {
		return err
	} else if found {
		if existing.Provider != p.ID || existing.EffectiveAuth() == nil || existing.EffectiveAuth().Kind != method {
			return errors.New("plan already exists with different provider or authentication")
		}
		return loginGatewayExisting(existing, method, fromStdin, input, out, loginExisting)
	}
	if p.DefaultBaseURL == "" || len(p.DefaultEndpoints) == 0 {
		return errors.New("provider has no default endpoint; configure a plan manually")
	}
	refs := gatewaySIWCStatusEntry{PlanID: planID, SessionRef: planID + "_siwc_session", HostIDRef: planID + "_siwc_host"}
	keyRef := planID + "_api_key"
	if secret.ValidRef(keyRef) != nil || secret.ValidRef(refs.SessionRef) != nil || secret.ValidRef(refs.HostIDRef) != nil {
		return errors.New("plan ID is too long for generated secret references")
	}
	endpoints := map[string]any{}
	for _, ep := range p.DefaultEndpoints {
		if method != provider.OAuthSIWC || ep == config.ProtocolOpenAIResponses {
			endpoints[ep] = map[string]string{"baseUrl": p.DefaultBaseURL}
		}
	}
	auth := map[string]string{"kind": string(method)}
	if method == provider.APIKey {
		auth["ref"] = keyRef
	} else {
		auth["sessionRef"], auth["hostIdRef"] = refs.SessionRef, refs.HostIDRef
	}
	models := map[string]any{}
	for _, model := range p.DefaultModels {
		models[model] = map[string]any{}
	}
	placeholder := len(models) == 0
	if placeholder {
		models["replace-with-model-id"] = map[string]any{}
	}
	body, err := yaml.Marshal(map[string]any{"version": 1, "id": planID, "provider": p.ID, "auth": auth, "endpoints": endpoints, "models": models})
	if err != nil {
		return errors.New("prepare gateway plan failed")
	}
	if _, diags := config.ValidatePlanBytes(planID+".yaml", planID, body); len(diags) != 0 {
		return errors.New("provider defaults cannot form a valid gateway plan")
	}
	newRefs := []string{keyRef}
	if method == provider.OAuthSIWC {
		newRefs = []string{refs.SessionRef, refs.HostIDRef}
	}
	secretDir := gatewayAuthStore().Secrets.Dir
	for _, ref := range newRefs {
		_, err := os.Lstat(filepath.Join(secretDir, ref))
		if err == nil {
			return errors.New("generated secret reference already exists; choose a different --plan")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return errors.New("check gateway secret reference failed")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(secretDir, 0700); err != nil {
		return errors.New("create gateway secret directory failed")
	}
	if err := os.Chmod(secretDir, 0700); err != nil {
		return errors.New("protect gateway secret directory failed")
	}
	// Keep new credentials private until authentication succeeds. Staging here
	// allows publication with hard links that cannot replace existing secrets.
	pendingDir, err := os.MkdirTemp(secretDir, ".gateway-auth-*")
	if err != nil {
		return errors.New("stage gateway authentication secrets failed")
	}
	defer func() {
		if cleanupErr := os.RemoveAll(pendingDir); cleanupErr != nil {
			err = errors.Join(err, errors.New("remove staged gateway authentication secrets failed"))
		}
	}()
	pending := siwc.Store{Secrets: secret.New(pendingDir)}
	if method == provider.APIKey {
		if err := saveGatewayAPIKey(pending.Secrets, keyRef, fromStdin, input); err != nil {
			return err
		}
	} else if err := login(pending, refs); err != nil {
		return err
	}
	if err := createGatewayAuthPlan(ctx, planID, body, pendingDir, secretDir, newRefs); err != nil {
		return err
	}
	if method == provider.OAuthSIWC {
		fmt.Fprintln(out, gatewayAuthSessionSaved)
	}
	fmt.Fprintln(out, "Gateway plan created.")
	if placeholder {
		fmt.Fprintln(out, "Replace the placeholder model in the plan with a model ID before use.")
	}
	return nil
}

func readGatewayAuthPlan(id string) (*config.Plan, bool, error) {
	path := filepath.Join(expandPath("~/.crabbro"), "gateway", "plans", id+".yaml")
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false, errors.New("cannot safely read gateway plan")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, errors.New("cannot read gateway plan")
	}
	p, diags := config.ValidatePlanBytes(id+".yaml", id, b)
	if len(diags) != 0 {
		return nil, false, errors.New("existing gateway plan is invalid")
	}
	return p, true, nil
}

func loginGatewayExisting(p *config.Plan, method provider.AuthKind, fromStdin bool, input io.Reader, out io.Writer, login func(gatewaySIWCStatusEntry) error) error {
	a := p.EffectiveAuth()
	if a == nil || (method != "" && method != a.Kind) {
		return errors.New("authentication method does not match existing plan")
	}
	switch a.Kind {
	case provider.OAuthSIWC:
		if fromStdin {
			return errors.New("--stdin requires api-key authentication")
		}
		return login(gatewaySIWCStatusEntry{PlanID: p.ID, SessionRef: a.SessionRef, HostIDRef: a.HostIDRef})
	case provider.APIKey:
		if err := saveGatewayAPIKey(gatewayAuthStore().Secrets, a.Ref, fromStdin, input); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "Gateway API key saved.")
		return err
	default:
		return errors.New("existing plan authentication method cannot be used with login")
	}
}

func saveGatewayAPIKey(store *secret.Store, ref string, fromStdin bool, input io.Reader) error {
	if !fromStdin {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return errors.New("open terminal failed; use --stdin")
		}
		defer tty.Close()
		state := exec.Command("stty", "-g")
		state.Stdin = tty
		old, err := state.Output()
		if err != nil {
			return errors.New("read terminal settings failed")
		}
		disable := exec.Command("stty", "-echo")
		disable.Stdin = tty
		if err := disable.Run(); err != nil {
			return errors.New("disable terminal echo failed")
		}
		defer func() {
			restore := exec.Command("stty", strings.TrimSpace(string(old)))
			restore.Stdin = tty
			_ = restore.Run()
			fmt.Fprintln(tty)
		}()
		fmt.Fprint(tty, "API key: ")
		input = tty
	}
	// Read a single bounded line without waiting for EOF on a terminal.
	var data []byte
	var b [1]byte
	for len(data) <= 65536 {
		n, err := input.Read(b[:])
		if n > 0 {
			if b[0] == '\n' {
				break
			}
			data = append(data, b[0])
		}
		if err != nil {
			if err != io.EOF {
				return errors.New("read API key failed")
			}
			break
		}
	}
	if len(data) > 65536 || strings.TrimSpace(string(data)) == "" {
		return errors.New("invalid API key input")
	}
	if err := store.Set(ref, strings.TrimSpace(string(data))); err != nil {
		return errors.New("save gateway API key failed")
	}
	return nil
}

func createGatewayAuthPlan(ctx context.Context, id string, body []byte, pendingDir, secretDir string, refs []string) (err error) {
	dir := filepath.Join(expandPath("~/.crabbro"), "gateway", "plans")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("create gateway plan directory failed")
	}
	f, err := os.CreateTemp(dir, ".plan-*")
	if err != nil {
		return errors.New("create gateway plan failed")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err != nil {
		f.Close()
		return errors.New("write gateway plan failed")
	}
	if err = f.Close(); err != nil {
		return errors.New("write gateway plan failed")
	}
	// Publish secrets before the plan so readers never see a plan whose
	// credentials are still staged. Roll back only links made by this attempt.
	published := 0
	defer func() {
		if err == nil {
			return
		}
		for _, ref := range refs[:published] {
			if rollbackErr := removePublishedGatewaySecret(filepath.Join(pendingDir, ref), filepath.Join(secretDir, ref)); rollbackErr != nil {
				err = errors.Join(err, rollbackErr)
			}
		}
	}()
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Link(filepath.Join(pendingDir, ref), filepath.Join(secretDir, ref)); err != nil {
			if errors.Is(err, os.ErrExist) {
				return errors.New("generated secret reference already exists; choose a different --plan")
			}
			return errors.New("publish gateway authentication secret failed")
		}
		published++
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err = os.Link(f.Name(), filepath.Join(dir, id+".yaml")); err != nil {
		return errors.New("gateway plan already exists or could not be created")
	}
	return nil
}

func removePublishedGatewaySecret(stagedPath, publishedPath string) error {
	staged, stagedErr := os.Lstat(stagedPath)
	published, err := os.Lstat(publishedPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if stagedErr != nil || err != nil {
		return errors.New("check published gateway authentication secret failed")
	}
	if os.SameFile(staged, published) {
		if err := os.Remove(publishedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("remove published gateway authentication secret failed")
		}
	}
	return nil
}
