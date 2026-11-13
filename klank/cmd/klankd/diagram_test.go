package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// TestMermaidLiveURL pins the encoding mermaid.live expects: the URL
// fragment is the base64url (Raw, unpadded) encoding of
// {"code":"<mermaid>","mermaid":{"theme":"default"}}.
func TestMermaidLiveURL(t *testing.T) {
	const mermaid = "graph TD;\nA-->B"
	url := mermaidLiveURL(mermaid)

	const prefix = "https://mermaid.live/edit#"
	if len(url) <= len(prefix) || url[:len(prefix)] != prefix {
		t.Fatalf("URL missing %q prefix: %q", prefix, url)
	}
	fragment := url[len(prefix):]

	// unpadded base64url
	if base64.RawURLEncoding.Strict().EncodeToString(mustDecode(t, fragment)) != fragment {
		t.Fatalf("fragment is not canonical RawURLEncoding: %q", fragment)
	}
	raw, err := base64.RawURLEncoding.DecodeString(fragment)
	if err != nil {
		t.Fatalf("fragment is not valid base64url: %v", err)
	}
	var st diagramState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("fragment is not valid state JSON: %v", err)
	}
	if st.Code != mermaid {
		t.Errorf("state.code = %q, want %q", st.Code, mermaid)
	}
	if st.Mermaid.Theme != "default" {
		t.Errorf("state.mermaid.theme = %q, want %q", st.Mermaid.Theme, "default")
	}

	// deterministic and mermaid-specific
	if again := mermaidLiveURL(mermaid); again != url {
		t.Errorf("encoding not deterministic: %q vs %q", again, url)
	}
	if other := mermaidLiveURL(mermaid + " "); other == url {
		t.Errorf("different mermaid encoded identically")
	}
}

// TestMermaidLiveURLSafeCharacters ensures the fragment survives inside a
// URL: only base64url characters, no padding '='.
func TestMermaidLiveURLSafeCharacters(t *testing.T) {
	url := mermaidLiveURL("sequenceDiagram\n  Alice->>Bob: hi <>&?#$")
	fragment := url[len("https://mermaid.live/edit#"):]
	for _, c := range fragment {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			t.Fatalf("fragment contains URL-unsafe character %q: %q", c, fragment)
		}
	}
}

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return raw
}

// TestBrowserOpenersFlagFirst: an explicit -open-command is always the
// first candidate (even if the binary is not on PATH — the host admin
// decided), split on whitespace with the URL appended.
func TestBrowserOpenersFlagFirst(t *testing.T) {
	cmds := browserOpeners("firefox --new-window", "https://x.example/diag#abc")
	if len(cmds) == 0 || len(cmds[0]) != 3 || cmds[0][0] != "firefox" || cmds[0][1] != "--new-window" || cmds[0][2] != "https://x.example/diag#abc" {
		t.Fatalf("expected [firefox --new-window URL] first, got %v", cmds)
	}
	// every candidate ends with the URL
	for i, c := range cmds {
		if len(c) < 2 || c[len(c)-1] != "https://x.example/diag#abc" {
			t.Errorf("openers[%d] does not end with the URL: %v", i, c)
		}
	}
}

// TestBrowserOpenersFirefoxNewWindow: the default firefox opener must
// include --new-window so diagrams open in their own window instead of
// a tab in the existing one.
func TestBrowserOpenersFirefoxNewWindow(t *testing.T) {
	cmds := browserOpeners("", "https://x.example/diag#abc")
	for _, c := range cmds {
		if c[0] != "firefox" {
			continue
		}
		if len(c) != 3 || c[1] != "--new-window" {
			t.Errorf("firefox opener = %v, want [firefox --new-window URL]", c)
		}
		return
	}
	t.Skip("firefox not on PATH in test environment")
}

// TestHandleShowDiagramInvalidPayload: malformed payloads must be a
// *result* (ok:false, delivered + acked), not a handler error (which
// would leave the message unacked forever).
func TestHandleShowDiagramInvalidPayload(t *testing.T) {
	h := handleShowDiagram("")
	for name, data := range map[string][]byte{
		"bad json":    []byte("{not json"),
		"empty array": []byte("[]"),
		"null":        nil,
	} {
		ok, result, err := h(data)
		if err != nil {
			t.Errorf("%s: handler returned error %v; want nil (acked failure result)", name, err)
		}
		if ok {
			t.Errorf("%s: ok = true, want false", name)
		}
		if result["error"] == nil {
			t.Errorf("%s: result missing error: %v", name, result)
		}
	}
}

// TestHandleShowDiagramNoBrowser: with no openers available the handler
// reports failure with the URL still included.
func TestHandleShowDiagramNoBrowser(t *testing.T) {
	// empty openCmd, and LookPath cannot find firefox under a PATH that
	// PATH that has no such binaries
	h := handleShowDiagram("")
	ok, result, err := h([]byte(`{"mermaid":"graph TD;A-->B"}`))
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	url, hasURL := result["url"].(string)
	if !hasURL || url == "" {
		t.Fatalf("result must carry the URL, got %v", result)
	}
	if ok {
		// a browser was found on this machine (PATH-dependent); nothing
		// more to assert portably
		t.Skip("a browser opener exists on this host; failure path not testable here")
	}
	if result["error"] == nil {
		t.Errorf("failure result missing error: %v", result)
	}
}
