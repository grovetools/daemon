package sync

import "testing"

func TestScanForSecrets(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantName string
		wantHit  bool
	}{
		{
			name:     "openrouter key",
			content:  "config: openrouter.api_key = sk-or-v1-abcdef0123456789ABCDEF01",
			wantName: "openrouter key",
			wantHit:  true,
		},
		{
			name:     "anthropic key locks existing behavior",
			content:  "export ANTHROPIC_API_KEY=sk-ant-abcdef0123456789ABCDEF01",
			wantName: "anthropic key",
			wantHit:  true,
		},
		{
			name:     "plain prose is clean",
			content:  "This document explains how to rotate the OpenRouter API key safely.",
			wantName: "",
			wantHit:  false,
		},
		{
			name:     "short openrouter fragment below threshold",
			content:  "the placeholder token sk-or-v1-abc is not a real key",
			wantName: "",
			wantHit:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotHit := ScanForSecrets([]byte(tc.content))
			if gotHit != tc.wantHit || gotName != tc.wantName {
				t.Errorf("ScanForSecrets() = (%q, %v), want (%q, %v)", gotName, gotHit, tc.wantName, tc.wantHit)
			}
		})
	}
}
