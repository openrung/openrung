// SPDX-License-Identifier: GPL-3.0-or-later

package relay

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeOperatorContact(t *testing.T) {
	valid := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"ops@example.org", "ops@example.org"},
		{"  ops@example.org\n", "ops@example.org"},
		{"ops+relay@mail.example.co.uk", "ops+relay@mail.example.co.uk"},
		{strings.Repeat("a", MaxOperatorContactLength-len("@example.org")) + "@example.org", strings.Repeat("a", MaxOperatorContactLength-len("@example.org")) + "@example.org"},
	}
	for _, c := range valid {
		got, err := NormalizeOperatorContact(c.in)
		if err != nil || got != c.want {
			t.Errorf("NormalizeOperatorContact(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	invalid := []string{
		strings.Repeat("a", MaxOperatorContactLength-len("@example.org")+1) + "@example.org",
		"telegram-handle",
		"@example.org",
		"ops@",
		"ops@example",
		"ops@.example.org",
		"ops@example.org.",
		"ops@ops@example.org",
		"opérateur@example.org",
		"ops @example.org",
		"ops@example.org\tx",
		"ops\x00@example.org",
		"ops\x1b[31m@example.org",
		"ops\u0085@example.org",
		"ops @example.org",
		"ops\xff@example.org",
	}
	for _, in := range invalid {
		if got, err := NormalizeOperatorContact(in); err == nil {
			t.Errorf("NormalizeOperatorContact(%q) = %q, want an error", in, got)
		}
	}
}

// TestOperatorContactIsRegistrationOnly pins the wire contract: the contact
// travels in the registration request but never in a serialized descriptor
// (public directory, signed lists, registration response).
func TestOperatorContactIsRegistrationOnly(t *testing.T) {
	body, err := json.Marshal(RegisterRequest{OperatorContact: "ops@example.org"})
	if err != nil {
		t.Fatalf("marshal register request: %v", err)
	}
	if !strings.Contains(string(body), `"operator_contact":"ops@example.org"`) {
		t.Fatalf("register request omitted operator_contact: %s", body)
	}
	if body, _ := json.Marshal(RegisterRequest{}); strings.Contains(string(body), "operator_contact") {
		t.Fatalf("empty operator_contact must be omitted: %s", body)
	}

	desc := Descriptor{ID: "relay", OperatorContact: "ops@example.org"}
	for name, value := range map[string]any{"descriptor": desc, "register response": RegisterResponse{Descriptor: desc}, "list": ListResponse{Relays: []Descriptor{desc}}} {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		if strings.Contains(string(body), "operator_contact") || strings.Contains(string(body), "ops@example.org") {
			t.Errorf("%s leaked the operator contact: %s", name, body)
		}
	}
}
