package config

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// langfuseRefs is a TelemetryConfig carrying both Langfuse key references.
func langfuseRefs() TelemetryConfig {
	return TelemetryConfig{
		LangfusePublicKeyRef: "secret://LANGFUSE_PUBLIC_KEY",
		LangfuseSecretKeyRef: "secret://LANGFUSE_SECRET_KEY",
	}
}

// TestValidateTelemetryAcceptsValidShapes: each supported destination
// validates for a deep-research run and a worker run alike.
func TestValidateTelemetryAcceptsValidShapes(t *testing.T) {
	withEndpoint := func(endpoint string) TelemetryConfig {
		tc := langfuseRefs()
		tc.LangfuseEndpoint = endpoint
		return tc
	}
	for _, tt := range []struct {
		name string
		tc   TelemetryConfig
	}{
		{"unset", TelemetryConfig{}},
		{"otlp https", TelemetryConfig{OTLPEndpoint: "https://otel.example/"}},
		{"otlp loopback http", TelemetryConfig{OTLPEndpoint: "http://127.0.0.1:4318"}},
		{"langfuse keys only", langfuseRefs()},
		{"langfuse self-hosted", withEndpoint("https://langfuse.internal/api/public/otel")},
		{"langfuse localhost", withEndpoint("http://localhost:3000/api/public/otel")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, cfg := range []ResearchConfig{Default(), validWorker()} {
				cfg.Telemetry = tt.tc
				if err := cfg.Validate(); err != nil {
					t.Errorf("agent %s: %v", cfg.Agent, err)
				}
			}
		})
	}
}

// TestValidateTelemetryRejects: every rejection names the offending field
// and never echoes the value, which may be a pasted credential.
func TestValidateTelemetryRejects(t *testing.T) {
	const literal = "sk-lf-6d5c4b3a-2f1e-0d9c-8b7a-6f5e4d3c2b1a"
	for _, tt := range []struct {
		name   string
		mutate func(*TelemetryConfig)
		want   string
	}{
		{"literal public key", func(tc *TelemetryConfig) {
			*tc = langfuseRefs()
			tc.LangfusePublicKeyRef = literal
		}, "telemetry.langfuse_public_key_ref"},
		{"literal secret key", func(tc *TelemetryConfig) {
			*tc = langfuseRefs()
			tc.LangfuseSecretKeyRef = literal
		}, "telemetry.langfuse_secret_key_ref"},
		{"otlp userinfo", func(tc *TelemetryConfig) {
			tc.OTLPEndpoint = "https://user:" + literal + "@otel.example"
		}, "telemetry.otlp_endpoint"},
		{"otlp query", func(tc *TelemetryConfig) {
			tc.OTLPEndpoint = "https://otel.example/?token=" + literal
		}, "telemetry.otlp_endpoint"},
		{"otlp cleartext non-loopback", func(tc *TelemetryConfig) {
			tc.OTLPEndpoint = "http://otel.example:4318"
		}, "telemetry.otlp_endpoint"},
		{"otlp not http", func(tc *TelemetryConfig) {
			tc.OTLPEndpoint = "grpc://otel.example:4317"
		}, "telemetry.otlp_endpoint"},
		{"langfuse fragment", func(tc *TelemetryConfig) {
			*tc = langfuseRefs()
			tc.LangfuseEndpoint = "https://langfuse.example/api/public/otel#" + literal
		}, "telemetry.langfuse_endpoint"},
		{"langfuse userinfo", func(tc *TelemetryConfig) {
			*tc = langfuseRefs()
			tc.LangfuseEndpoint = "https://pk:" + literal + "@langfuse.example/api/public/otel"
		}, "telemetry.langfuse_endpoint"},
		{"langfuse cleartext non-loopback", func(tc *TelemetryConfig) {
			*tc = langfuseRefs()
			tc.LangfuseEndpoint = "http://langfuse.internal/api/public/otel"
		}, "telemetry.langfuse_endpoint"},
		{"public key without secret key", func(tc *TelemetryConfig) {
			tc.LangfusePublicKeyRef = "secret://LANGFUSE_PUBLIC_KEY"
		}, "telemetry.langfuse_secret_key_ref: is required"},
		{"secret key without public key", func(tc *TelemetryConfig) {
			tc.LangfuseSecretKeyRef = "secret://LANGFUSE_SECRET_KEY"
		}, "telemetry.langfuse_public_key_ref: is required"},
		{"langfuse endpoint without keys", func(tc *TelemetryConfig) {
			tc.LangfuseEndpoint = DefaultLangfuseEndpoint
		}, "telemetry.langfuse_endpoint: needs"},
		{"otlp and langfuse together", func(tc *TelemetryConfig) {
			*tc = langfuseRefs()
			tc.OTLPEndpoint = "https://otel.example"
		}, "one destination"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, cfg := range []ResearchConfig{Default(), validWorker()} {
				tt.mutate(&cfg.Telemetry)
				err := cfg.Validate()
				if err == nil {
					t.Fatalf("agent %s: validated", cfg.Agent)
				}
				if !strings.Contains(err.Error(), tt.want) {
					t.Errorf("agent %s: err = %v, want it to contain %q", cfg.Agent, err, tt.want)
				}
				if strings.Contains(err.Error(), literal) {
					t.Errorf("agent %s: validation error leaked the literal: %v", cfg.Agent, err)
				}
			}
		})
	}
}

// TestLangfuseTarget: the default endpoint applies only when both key
// references are set and no endpoint is given.
func TestLangfuseTarget(t *testing.T) {
	custom := langfuseRefs()
	custom.LangfuseEndpoint = "https://langfuse.internal/api/public/otel"
	for _, tt := range []struct {
		name string
		tc   TelemetryConfig
		want string
	}{
		{"unset", TelemetryConfig{}, ""},
		{"otlp only", TelemetryConfig{OTLPEndpoint: "https://otel.example"}, ""},
		{"public key only", TelemetryConfig{LangfusePublicKeyRef: "secret://P"}, ""},
		{"keys only", langfuseRefs(), DefaultLangfuseEndpoint},
		{"keys and endpoint", custom, custom.LangfuseEndpoint},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.tc.LangfuseTarget(); got != tt.want {
				t.Errorf("LangfuseTarget() = %q, want %q", got, tt.want)
			}
		})
	}
	if Default().Telemetry != (TelemetryConfig{}) {
		t.Errorf("Default() sets telemetry: %+v", Default().Telemetry)
	}
}

// TestApplyFlagsTelemetry pins each telemetry flag to its own field: set
// alone, only that field may change from Default().
func TestApplyFlagsTelemetry(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want TelemetryConfig
	}{
		{"otlp-endpoint", []string{"--otlp-endpoint", "https://otel.example"}, TelemetryConfig{OTLPEndpoint: "https://otel.example"}},
		{"langfuse-endpoint", []string{"--langfuse-endpoint", "https://langfuse.internal/api/public/otel"}, TelemetryConfig{LangfuseEndpoint: "https://langfuse.internal/api/public/otel"}},
		{"langfuse-public-key-ref", []string{"--langfuse-public-key-ref", "secret://LF_PUBLIC"}, TelemetryConfig{LangfusePublicKeyRef: "secret://LF_PUBLIC"}},
		{"langfuse-secret-key-ref", []string{"--langfuse-secret-key-ref", "secret://LF_SECRET"}, TelemetryConfig{LangfuseSecretKeyRef: "secret://LF_SECRET"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			fs := newFlagSet(t)
			if err := fs.Parse(tt.args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := ApplyFlags(&cfg, fs); err != nil {
				t.Fatalf("ApplyFlags: %v", err)
			}
			if cfg.Telemetry != tt.want {
				t.Errorf("Telemetry = %+v, want %+v", cfg.Telemetry, tt.want)
			}
			rest := cfg
			rest.Telemetry = TelemetryConfig{}
			if !reflect.DeepEqual(rest, Default()) {
				t.Errorf("an unrelated field changed: %+v", cfg)
			}
		})
	}
}

// TestApplyFlagsTelemetryLayersOverBase: a base config's telemetry block
// survives unset flags, and a set flag overrides only its own field.
func TestApplyFlagsTelemetryLayersOverBase(t *testing.T) {
	const base = "telemetry:\n" +
		"  langfuse_endpoint: https://langfuse.internal/api/public/otel\n" +
		"  langfuse_public_key_ref: secret://FILE_PUBLIC\n" +
		"  langfuse_secret_key_ref: secret://FILE_SECRET\n"
	for _, tt := range []struct {
		name string
		args []string
		want TelemetryConfig
	}{
		{"unrelated flag", []string{"--visualise"}, TelemetryConfig{
			LangfuseEndpoint:     "https://langfuse.internal/api/public/otel",
			LangfusePublicKeyRef: "secret://FILE_PUBLIC",
			LangfuseSecretKeyRef: "secret://FILE_SECRET",
		}},
		{"one telemetry flag", []string{"--langfuse-secret-key-ref", "secret://FLAG_SECRET"}, TelemetryConfig{
			LangfuseEndpoint:     "https://langfuse.internal/api/public/otel",
			LangfusePublicKeyRef: "secret://FILE_PUBLIC",
			LangfuseSecretKeyRef: "secret://FLAG_SECRET",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Decode(strings.NewReader(base))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			fs := newFlagSet(t)
			if err := fs.Parse(tt.args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := ApplyFlags(&cfg, fs); err != nil {
				t.Fatalf("ApplyFlags: %v", err)
			}
			if cfg.Telemetry != tt.want {
				t.Errorf("Telemetry = %+v, want %+v", cfg.Telemetry, tt.want)
			}
		})
	}
}

// TestTelemetryRoundTrip: the telemetry block survives EncodeJSON -> Decode
// under its documented keys, and an unset block is omitted.
func TestTelemetryRoundTrip(t *testing.T) {
	var empty bytes.Buffer
	if err := Default().EncodeJSON(&empty); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	if strings.Contains(empty.String(), `"telemetry"`) {
		t.Errorf("an unset telemetry block is encoded:\n%s", empty.String())
	}

	in := Default()
	in.Telemetry = langfuseRefs()
	in.Telemetry.LangfuseEndpoint = "https://langfuse.internal/api/public/otel"
	var buf bytes.Buffer
	if err := in.EncodeJSON(&buf); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	for _, key := range []string{`"telemetry"`, `"langfuse_endpoint"`, `"langfuse_public_key_ref"`, `"langfuse_secret_key_ref"`} {
		if !strings.Contains(buf.String(), key) {
			t.Errorf("encoded config lacks %s:\n%s", key, buf.String())
		}
	}
	out, err := Decode(&buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.Telemetry != in.Telemetry {
		t.Errorf("round trip lost data:\n got %+v\nwant %+v", out.Telemetry, in.Telemetry)
	}

	otlp, err := Decode(strings.NewReader("telemetry:\n  otlp_endpoint: https://otel.example\n"))
	if err != nil {
		t.Fatalf("Decode YAML: %v", err)
	}
	if otlp.Telemetry.OTLPEndpoint != "https://otel.example" {
		t.Errorf("otlp_endpoint = %q", otlp.Telemetry.OTLPEndpoint)
	}
}
