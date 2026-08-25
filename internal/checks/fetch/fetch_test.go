package fetch_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/checks/fetch"
	"github.com/mehrnet/radar-node/internal/probe"
)

func TestDo_ReturnsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	body, status, err := fetch.Do(context.Background(), probe.Options{Target: srv.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if string(body) != "hello world" {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestDo_CapsBodySize(t *testing.T) {
	// 11MB of content, well over fetch's own 10MB cap.
	big := strings.Repeat("x", 11*1024*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	}))
	defer srv.Close()

	body, _, err := fetch.Do(context.Background(), probe.Options{Target: srv.URL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(body) != 10*1024*1024 {
		t.Fatalf("expected body capped at 10MB, got %d bytes", len(body))
	}
}

func TestCheck_Fails_OnUnreachable(t *testing.T) {
	c := fetch.New()
	res := c.Check(context.Background(), probe.Options{Target: "http://127.0.0.1:1", Timeout: 500 * time.Millisecond})
	if res.Ok {
		t.Fatal("expected failure for an unreachable target")
	}
}

func check(t *testing.T, target string, params map[string]any) probe.Result {
	t.Helper()
	c := fetch.New()
	if params == nil {
		params = map[string]any{}
	}
	return c.Check(context.Background(), probe.Options{Target: target, Timeout: 2 * time.Second, Seq: 1, Params: params})
}

func TestCheck_DefaultsToGET(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.Write([]byte("{}"))
	}))
	defer srv.Close()

	res := check(t, srv.URL, nil)
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if gotMethod != "GET" {
		t.Fatalf("expected GET, got %q", gotMethod)
	}
}

func TestCheck_POSTWithBody(t *testing.T) {
	var gotMethod, gotBody, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte("{}"))
	}))
	defer srv.Close()

	res := check(t, srv.URL, map[string]any{
		"method":  "post",
		"body":    `{"query":"asn"}`,
		"headers": map[string]any{"Content-Type": "application/json"},
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if gotMethod != "POST" {
		t.Fatalf("expected POST (case-insensitive method param), got %q", gotMethod)
	}
	if gotBody != `{"query":"asn"}` {
		t.Fatalf("expected body to reach the server, got %q", gotBody)
	}
	if gotContentType != "application/json" {
		t.Fatalf("expected Content-Type header to reach the server, got %q", gotContentType)
	}
}

func TestCheck_CustomHeadersForPrivateAPIAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte("{}"))
	}))
	defer srv.Close()

	res := check(t, srv.URL, map[string]any{"headers": map[string]any{"Authorization": "Bearer secret-key"}})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("expected the Authorization header to reach the server, got %q", gotAuth)
	}
}

func TestCheck_RejectsUnsupportedMethod(t *testing.T) {
	res := check(t, "http://example.invalid", map[string]any{"method": "TRACE"})
	if res.Ok {
		t.Fatal("expected failure for an unsupported method")
	}
}

func TestCheck_RejectsNonStringHeaderValue(t *testing.T) {
	res := check(t, "http://example.invalid", map[string]any{"headers": map[string]any{"X-Count": 5}})
	if res.Ok {
		t.Fatal("expected failure for a non-string header value")
	}
}

func TestCheck_RejectsTooManyHeaders(t *testing.T) {
	headers := map[string]any{}
	for i := 0; i < 25; i++ {
		headers[string(rune('a'+i))] = "v"
	}
	res := check(t, "http://example.invalid", map[string]any{"headers": headers})
	if res.Ok {
		t.Fatal("expected failure for too many headers")
	}
}

// jsonServer serves a fixed JSON body -- the shape a real IP/ASN
// lookup API would return, used across the field-extraction tests
// below.
func jsonServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheck_JQFieldExtraction(t *testing.T) {
	srv := jsonServer(t, `{"status":"success","country":"United States","as":"AS15169 Google LLC","lat":37.751}`)

	res := check(t, srv.URL, map[string]any{
		"fields": map[string]any{
			"asn":     map[string]any{"parser": "jq", "expr": ".as"},
			"country": map[string]any{"parser": "jq", "expr": ".country"},
			"lat":     map[string]any{"parser": "jq", "expr": ".lat"},
		},
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if res.Extra["asn"] != "AS15169 Google LLC" {
		t.Fatalf("expected asn field, got %v", res.Extra["asn"])
	}
	if res.Extra["country"] != "United States" {
		t.Fatalf("expected country field, got %v", res.Extra["country"])
	}
	if res.Extra["lat"] != 37.751 {
		t.Fatalf("expected lat field as a real number, got %v (%T)", res.Extra["lat"], res.Extra["lat"])
	}
	if res.Extra["http_code"] != http.StatusOK {
		t.Fatalf("expected http_code to still be reported, got %v", res.Extra["http_code"])
	}
}

func TestCheck_FieldAcceptsSingleStepShorthand(t *testing.T) {
	srv := jsonServer(t, `{"as":"AS15169"}`)
	res := check(t, srv.URL, map[string]any{
		"fields": map[string]any{"asn": map[string]any{"parser": "jq", "expr": ".as"}},
	})
	if !res.Ok || res.Extra["asn"] != "AS15169" {
		t.Fatalf("expected asn=AS15169, got ok=%v extra=%v error=%q", res.Ok, res.Extra, res.Error)
	}
}

func TestCheck_ChainedBase64ThenJQ(t *testing.T) {
	inner := `{"as":"AS15169 Google LLC"}`
	encoded := base64.StdEncoding.EncodeToString([]byte(inner))
	srv := jsonServer(t, encoded)

	res := check(t, srv.URL, map[string]any{
		"fields": map[string]any{
			"asn": []any{
				map[string]any{"parser": "base64"},
				map[string]any{"parser": "jq", "expr": ".as"},
			},
		},
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if res.Extra["asn"] != "AS15169 Google LLC" {
		t.Fatalf("expected chained base64->jq extraction, got %v", res.Extra["asn"])
	}
}

func TestCheck_Base64OnlyField(t *testing.T) {
	inner := "vless://uuid@host:443?type=tcp#name"
	encoded := base64.StdEncoding.EncodeToString([]byte(inner))
	srv := jsonServer(t, encoded)

	res := check(t, srv.URL, map[string]any{
		"fields": map[string]any{"decoded": map[string]any{"parser": "base64"}},
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if res.Extra["decoded"] != inner {
		t.Fatalf("expected decoded field to be the real decoded string (not base64-of-base64), got %v", res.Extra["decoded"])
	}
}

func TestCheck_RegexFieldExtraction(t *testing.T) {
	srv := jsonServer(t, "build_version=1.4.2 uptime=99.98%")
	res := check(t, srv.URL, map[string]any{
		"fields": map[string]any{
			"version": map[string]any{"parser": "regex", "expr": `build_version=(\S+)`},
		},
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if res.Extra["version"] != "1.4.2" {
		t.Fatalf("expected version=1.4.2, got %v", res.Extra["version"])
	}
}

func TestCheck_MisconfiguredFieldIsOmittedNotFatal(t *testing.T) {
	srv := jsonServer(t, `{"as":"AS15169"}`)
	res := check(t, srv.URL, map[string]any{
		"fields": map[string]any{
			"good": map[string]any{"parser": "jq", "expr": ".as"},
			"bad":  map[string]any{"parser": "jq", "expr": ".nonexistent.deeply.nested"},
		},
	})
	if !res.Ok {
		t.Fatalf("expected the overall check to stay ok despite one bad field, got error %q", res.Error)
	}
	if res.Extra["good"] != "AS15169" {
		t.Fatalf("expected the good field to still populate, got %v", res.Extra["good"])
	}
	if _, present := res.Extra["bad"]; present {
		t.Fatalf("expected the bad field to be omitted, got %v", res.Extra["bad"])
	}
}

func TestCheck_UnknownParserIsOmittedNotFatal(t *testing.T) {
	srv := jsonServer(t, `{"as":"AS15169"}`)
	res := check(t, srv.URL, map[string]any{
		"fields": map[string]any{"x": map[string]any{"parser": "yaml"}},
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if _, present := res.Extra["x"]; present {
		t.Fatalf("expected the field with an unsupported parser to be omitted, got %v", res.Extra["x"])
	}
}

func TestCheck_BuiltInFieldsWinOverSameNamedUserField(t *testing.T) {
	srv := jsonServer(t, `{"http_code":"not a real code"}`)
	res := check(t, srv.URL, map[string]any{
		"fields": map[string]any{"http_code": map[string]any{"parser": "jq", "expr": ".http_code"}},
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if res.Extra["http_code"] != http.StatusOK {
		t.Fatalf("expected the real numeric http_code to win, got %v (%T)", res.Extra["http_code"], res.Extra["http_code"])
	}
}

func TestCheck_RejectsTooManyFields(t *testing.T) {
	srv := jsonServer(t, `{}`)
	fields := map[string]any{}
	for i := 0; i < 25; i++ {
		fields[string(rune('a'+i))] = map[string]any{"parser": "jq", "expr": "."}
	}
	res := check(t, srv.URL, map[string]any{"fields": fields})
	if res.Ok {
		t.Fatal("expected failure for too many fields")
	}
}

func TestCheck_JQArrayResultStaysStructured(t *testing.T) {
	srv := jsonServer(t, `{"tags":["a","b","c"]}`)
	res := check(t, srv.URL, map[string]any{
		"fields": map[string]any{"tags": map[string]any{"parser": "jq", "expr": ".tags"}},
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	// Round-trip through JSON to confirm this is really a JSON-
	// marshalable structured value, not e.g. a Go-syntax fmt string.
	encoded, err := json.Marshal(res.Extra["tags"])
	if err != nil {
		t.Fatalf("expected tags to be JSON-marshalable, got error: %v", err)
	}
	if string(encoded) != `["a","b","c"]` {
		t.Fatalf("expected [\"a\",\"b\",\"c\"], got %s", encoded)
	}
}
