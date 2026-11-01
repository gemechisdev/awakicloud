package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gemechischala/awaki/internal/tools/types"
)

const (
	requestTimeout    = 30 * time.Second
	maxRequestTimeout = 120 * time.Second
	maxRequestBody    = 1 * 1024 * 1024
)

var allowedMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true,
}

// shown response headers (Set-Cookie is summarised, never echoed)
var shownResponseHeaders = []string{
	"Content-Type", "Content-Length", "Content-Disposition", "Location", "Retry-After",
	"ETag", "Last-Modified", "WWW-Authenticate", "X-RateLimit-Remaining", "X-RateLimit-Reset", "RateLimit-Remaining",
}

func WebRequestTool() types.Tool {
	return types.Tool{
		Name:  "web_request",
		Label: "Making request",
		Description: "Make an HTTP request (GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS) with custom headers. Returns the status, key response headers and body. " +
			"Use for REST APIs and webhooks. Send JSON with `json` (an object) or a raw string in `body`; `form` sends application/x-www-form-urlencoded; `query` adds URL parameters. " +
			"Binary responses are not dumped into the chat — use web_download for files. Bodies over 1 MB are cut and marked as truncated.",
		Parameters: map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The full URL to request",
			},
			"method": map[string]any{
				"type":        "string",
				"description": "HTTP method (default GET)",
			},
			"headers": map[string]any{
				"type":        "object",
				"description": `Optional HTTP headers, e.g. {"Authorization": "Bearer token"}`,
			},
			"query": map[string]any{
				"type":        "object",
				"description": "Optional URL query parameters as key/value pairs (merged with any already in the URL)",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "Raw request body text, sent as-is (e.g. a JSON string, XML, plain text). Content-Type defaults to application/json. For a JSON object prefer the json parameter.",
			},
			"json": map[string]any{
				"type":        "object",
				"description": "A JSON object body — serialised for you, Content-Type application/json (use instead of body)",
			},
			"form": map[string]any{
				"type":        "object",
				"description": "Form fields sent as application/x-www-form-urlencoded (instead of body)",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": "Timeout in seconds (default 30, max 120)",
			},
		},
		Required: []string{"url"},
		Handler:  webRequest,
	}
}

type requestSpec struct {
	URL         string
	Method      string
	Headers     map[string]string
	Body        []byte
	ContentType string // default Content-Type for Body, when the caller set none
	Timeout     time.Duration
}

func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case json.Number:
		return t.String(), true
	}
	return "", false
}

// buildRequestSpec turns tool input into a request. It is pure so every
// input shape can be unit-tested without a network.
func buildRequestSpec(input map[string]any) (requestSpec, error) {
	spec := requestSpec{Headers: map[string]string{}, Timeout: requestTimeout}
	raw, err := types.GetString(input, "url")
	if err != nil {
		return spec, err
	}
	spec.Method = strings.ToUpper(strings.TrimSpace(types.GetStringOpt(input, "method")))
	if spec.Method == "" {
		spec.Method = "GET"
	}
	if !allowedMethods[spec.Method] {
		return spec, fmt.Errorf("method %q is not supported (use GET, POST, PUT, PATCH, DELETE, HEAD or OPTIONS)", spec.Method)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return spec, fmt.Errorf("bad url: %v", err)
	}
	if q, ok := input["query"].(map[string]any); ok && len(q) > 0 {
		vals := u.Query()
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if s, ok := scalarString(q[k]); ok {
				vals.Set(k, s)
			} else if arr, ok := q[k].([]any); ok {
				for _, item := range arr {
					if s, ok := scalarString(item); ok {
						vals.Add(k, s)
					}
				}
			}
		}
		u.RawQuery = vals.Encode()
	}
	spec.URL = u.String()

	if h, ok := input["headers"].(map[string]any); ok {
		for k, v := range h {
			s, ok := scalarString(v)
			if !ok {
				continue
			}
			if strings.ContainsAny(k, "\r\n:") || strings.ContainsAny(s, "\r\n") {
				return spec, fmt.Errorf("invalid header %q", k)
			}
			spec.Headers[http.CanonicalHeaderKey(k)] = s
		}
	}

	form, hasForm := input["form"].(map[string]any)
	body, hasBody := input["body"]
	if jv, ok := input["json"]; ok && jv != nil {
		if hasBody && body != nil && body != "" {
			return spec, fmt.Errorf("pass either body or json, not both")
		}
		body, hasBody = jv, true
	}
	if hasBody && body == nil {
		hasBody = false
	}
	if s, ok := body.(string); ok && s == "" {
		hasBody = false
	}
	switch {
	case hasForm && len(form) > 0 && hasBody:
		return spec, fmt.Errorf("pass only one of body, json or form")
	case hasForm && len(form) > 0:
		vals := url.Values{}
		for k, v := range form {
			if s, ok := scalarString(v); ok {
				vals.Set(k, s)
			}
		}
		spec.Body = []byte(vals.Encode())
		spec.ContentType = "application/x-www-form-urlencoded"
	case hasBody:
		if s, ok := body.(string); ok {
			spec.Body = []byte(s)
		} else {
			b, err := json.Marshal(body)
			if err != nil {
				return spec, fmt.Errorf("body is not serialisable: %v", err)
			}
			spec.Body = b
		}
		spec.ContentType = "application/json"
	}

	if t, ok := types.GetIntOpt(input, "timeout"); ok && t > 0 {
		spec.Timeout = time.Duration(t) * time.Second
		if spec.Timeout > maxRequestTimeout {
			spec.Timeout = maxRequestTimeout
		}
	}
	return spec, nil
}

func webRequest(ctx context.Context, input map[string]any) types.Result {
	spec, err := buildRequestSpec(input)
	if err != nil {
		return types.ErrResult(err)
	}
	if err := ValidatePublicURL(spec.URL); err != nil {
		return types.Errf("url blocked: %v", err)
	}
	return execRequest(ctx, safeHTTPClient(spec.Timeout), spec)
}

// execRequest performs the request with the given client (injectable for tests).
func execRequest(ctx context.Context, client *http.Client, spec requestSpec) types.Result {
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	var bodyReader io.Reader
	if len(spec.Body) > 0 {
		bodyReader = bytes.NewReader(spec.Body)
	}
	req, err := http.NewRequestWithContext(ctx, spec.Method, spec.URL, bodyReader)
	if err != nil {
		return types.Errf("bad request: %v", err)
	}
	req.Header.Set("User-Agent", "AwakiCloudBot/1.0")
	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}
	if len(spec.Body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", spec.ContentType)
	}

	resp, err := client.Do(req)
	if err != nil {
		return types.Errf("http error: %v", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRequestBody+1))
	if err != nil && len(data) == 0 {
		return types.Errf("read body error: %v", err)
	}
	truncated := len(data) > maxRequestBody
	if truncated {
		data = data[:maxRequestBody]
	}
	final := ""
	if resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.String() != spec.URL {
		final = resp.Request.URL.String()
	}
	out := formatResponse(resp, final, data, truncated, spec.Method == http.MethodHead)
	r := types.TruncateOutput(out)
	r.IsErr = resp.StatusCode >= 400
	return r
}

func looksBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	if bytes.IndexByte(data[:n], 0) >= 0 {
		return true
	}
	// A cut multi-byte rune at the end of the probe window is not evidence of binary data.
	probe := data[:n]
	for i := 0; i < 3 && len(probe) > 0 && !utf8.Valid(probe); i++ {
		probe = probe[:len(probe)-1]
	}
	return !utf8.Valid(probe)
}

func formatResponse(resp *http.Response, finalURL string, body []byte, truncated, head bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "HTTP %s\n", resp.Status)
	if finalURL != "" {
		fmt.Fprintf(&sb, "Final-URL: %s\n", finalURL)
	}
	for _, name := range shownResponseHeaders {
		if v := resp.Header.Get(name); v != "" {
			fmt.Fprintf(&sb, "%s: %s\n", name, v)
		}
	}
	if cookies := resp.Header.Values("Set-Cookie"); len(cookies) > 0 {
		names := make([]string, 0, len(cookies))
		for _, c := range cookies {
			name, _, _ := strings.Cut(c, "=")
			names = append(names, strings.TrimSpace(name))
		}
		fmt.Fprintf(&sb, "Set-Cookie: %d cookie(s) set (%s) — values hidden\n", len(cookies), strings.Join(names, ", "))
	}
	sb.WriteString("\n")
	switch {
	case head:
		sb.WriteString("(HEAD request: no body)")
	case len(body) == 0:
		sb.WriteString("(empty body)")
	case looksBinary(body):
		fmt.Fprintf(&sb, "[binary body: %d bytes, Content-Type %q — not shown. Use web_download to save it to a file.]", len(body), resp.Header.Get("Content-Type"))
	default:
		sb.Write(body)
		if truncated {
			fmt.Fprintf(&sb, "\n[response truncated: only the first %d bytes were read; more data exists]", maxRequestBody)
		}
	}
	return sb.String()
}
