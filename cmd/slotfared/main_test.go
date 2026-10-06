package main

import "testing"

func TestLedgerRequiredError(t *testing.T) {
	// The guard exists so a deployment that asks for on-chain enforcement
	// cannot be handed the Memo rail instead: required without a program is
	// an error, every other combination is fine.
	cases := []struct {
		name     string
		required bool
		enabled  bool
		wantErr  bool
	}{
		{name: "required with a program is allowed", required: true, enabled: true},
		{name: "required without a program is refused", required: true, enabled: false, wantErr: true},
		{name: "not required without a program is the Memo rail", required: false, enabled: false},
		{name: "not required with a program is the ledger rail", required: false, enabled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ledgerRequiredError(tc.required, tc.enabled)
			if tc.wantErr && err == nil {
				t.Fatal("want an error refusing the Memo rail, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}

func TestAdvertisedBaseURL(t *testing.T) {
	cases := []struct {
		name      string
		publicURL string
		listen    string
		prefix    string
		want      string
		wantErr   bool
	}{
		{
			name:      "public URL composes the split-origin base",
			publicURL: "https://testnet-api-slotfare.svrnty.io",
			listen:    "127.0.0.1:8443",
			prefix:    "/infer",
			want:      "https://testnet-api-slotfare.svrnty.io/infer/v1",
		},
		{
			name:      "trailing slash on the public URL is trimmed before composing",
			publicURL: "https://testnet-api-slotfare.svrnty.io/",
			listen:    "127.0.0.1:8443",
			prefix:    "/infer",
			want:      "https://testnet-api-slotfare.svrnty.io/infer/v1",
		},
		{
			name:   "unset public URL derives from listen",
			listen: "127.0.0.1:8443",
			prefix: "/infer",
			want:   "http://127.0.0.1:8443/infer/v1",
		},
		{
			name:   "bare port listen derives 127.0.0.1",
			listen: "9999",
			prefix: "/infer",
			want:   "http://127.0.0.1:9999/infer/v1",
		},
		{
			name:      "no scheme is rejected",
			publicURL: "testnet-api-slotfare.svrnty.io",
			prefix:    "/infer",
			wantErr:   true,
		},
		{
			name:      "missing host is rejected",
			publicURL: "https://",
			prefix:    "/infer",
			wantErr:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := advertisedBaseURL(c.publicURL, c.listen, c.prefix)
			if c.wantErr {
				if err == nil {
					t.Fatalf("advertisedBaseURL(%q) = %q, want error", c.publicURL, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("advertisedBaseURL(%q): %v", c.publicURL, err)
			}
			if got != c.want {
				t.Errorf("advertisedBaseURL(%q) = %q, want %q", c.publicURL, got, c.want)
			}
		})
	}
}
