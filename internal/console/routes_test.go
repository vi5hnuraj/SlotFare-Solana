package console

import "testing"

func TestAllowedMethods(t *testing.T) {
	cases := []struct {
		path string
		want []string
	}{
		{"/session/nonce", []string{"POST"}},
		{"/session/verify", []string{"POST"}},
		{"/session/secret", []string{"POST"}},
		{"/session/signout", []string{"POST"}},
		{"/keys", []string{"GET", "POST"}},
		{"/keys/abc", []string{"DELETE"}},
		{"/account", []string{"GET"}},
		{"/usage", []string{"GET"}},
		{"/funding", []string{"GET"}},
		{"/chains", []string{"GET"}},
		{"/market", []string{"GET"}},
		{"/listings", []string{"GET", "POST"}},
		{"/listings/7/cancel", []string{"POST"}},
		{"/listings/7/pay", []string{"POST"}},
		{"/models", []string{"GET"}},
		{"/deposit-target", []string{"GET"}},
		{"/stream", []string{"GET"}},
		{"/attestation", []string{"GET"}},
		{"/unknown", nil},
		{"/", nil},
		{"/session/other", nil},
	}
	for _, c := range cases {
		got := AllowedMethods(c.path)
		if len(got) != len(c.want) {
			t.Errorf("AllowedMethods(%q) = %v, want %v", c.path, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("AllowedMethods(%q) = %v, want %v", c.path, got, c.want)
			}
		}
	}
}
