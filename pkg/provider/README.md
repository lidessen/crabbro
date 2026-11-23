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
Unverifiable thinking signatures are stripped even on the same protocol because
the stateless client has no signature provenance. `DoStream` writes SSE to a
caller-owned writer, honors request cancellation, and returns accumulated usage
when supplied by the upstream. Decision requests use `SystemOne`,
`OpenAIDecisions`, or `GeminiDecisions`; streaming decisions are unsupported.

A non-2xx response returns both a `Response` containing the original status,
headers and body and an `*HTTPError`. Transport, resolver, decoding and writer
errors are returned to the caller. The caller owns all credential storage and
must not put tokens into endpoint URLs, static headers, or logs.
