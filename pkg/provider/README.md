# Provider client

`pkg/provider` is a stateless HTTP and wire-protocol bridge. It does not load
Crabbro configuration or credentials, start listeners, or persist usage. A
caller supplies an endpoint, an HTTP client (or a client with a custom
`Transport`), and a resolver when credentials are needed.

```go
client := &provider.Client{
    HTTPClient: http.DefaultClient,
    Resolver: provider.CredentialResolverFunc(func(ctx context.Context, ref string) (string, error) {
        return myCredentialStore.Resolve(ctx, ref)
    }),
}
result, err := client.Do(ctx, provider.Endpoint{
    BaseURL: "https://api.example.com",
    Protocol: provider.OpenAIChat,
    Model: "model-name",
    CredentialRef: "secret://example",
}, provider.Request{
    Protocol: provider.Anthropic,
    Model: "requested-model",
    Body: requestJSON,
})
```

The input and output bodies use the request protocol; `Endpoint.Protocol`
selects the upstream protocol. Equal protocols preserve vendor-specific fields,
while differing protocols translate through the shared intermediate format.
Unverifiable thinking signatures are stripped even on the same protocol. To
replay verified Anthropic thinking or Gemini thought signatures, set a stable,
nonempty `Endpoint.SignaturePlanID`, create a `SignatureProvenance` with
`NewSignatureProvenance`, and pass it as `Request.SignatureProvenance`. Record
each signature or redacted blob with `Record(value, endpoint.SignatureOrigin())`,
or record a complete upstream response with `RecordResponse`. The caller must
associate each value with the endpoint that actually produced it, including for
streamed responses. Only values recorded for the exact plan, endpoint URL,
protocol, and model are retained; unknown or mismatched values are stripped.
The registry is bounded and in memory, so forgotten values are stripped too.
`DoStream` writes SSE to a
caller-owned writer, honors request cancellation, and returns accumulated usage
when supplied by the upstream. Decision requests use `SystemOne`,
`OpenAIDecisions`, or `GeminiDecisions`; streaming decisions are unsupported.

A non-2xx response returns both a `Response` containing the upstream status, headers and body (after any configured response normalization) and an `*HTTPError`. Transport, resolver, decoding and writer
errors are returned to the caller. The caller owns all credential storage and
must not put tokens into endpoint URLs, static headers, or logs.

`Endpoint.RequestTransform` and `Endpoint.ModelRequestTransform` accept the same
normalization operations as gateway plan transforms. They run in that order
after encoding and before credential injection. See the gateway example in the
[main README](../../README.md#llm--decision-gateway).

`Endpoint.ResponseTransform` and `Endpoint.ModelResponseTransform` run in that
order on buffered non-SSE responses, including HTTP errors. `unwrapPath` and
`extractField` are alternative dot-separated paths selecting a JSON object;
then fields are dropped, renamed, and set. Missing or non-object selections
fail the request. Response bodies are limited to 16 MiB before and after
transformation. SSE streams are not transformed.
