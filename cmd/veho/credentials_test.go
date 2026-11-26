package main

import (
	"encoding/json"
	"testing"
)

// FuzzParseCredentials: any input is refused or yields complete logins
// that encode again.
func FuzzParseCredentials(f *testing.F) {
	f.Add([]byte(`{"logins": {"https://a:1": {"issuer": "https://i", "client_id": "c", "access_token": "t"}}}`))
	f.Add([]byte(`{"logins": {"https://a:1": null}}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := parseCredentials(b)
		if err != nil {
			return
		}
		for url, l := range c.Logins {
			if l == nil || l.Issuer == "" || l.ClientID == "" || l.Access == "" {
				t.Fatalf("an incomplete login of %q accepted", url)
			}
		}
		if _, err := json.Marshal(c); err != nil {
			t.Fatal(err)
		}
	})
}
