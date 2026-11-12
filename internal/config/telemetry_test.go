package config

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// langfuseRefs is a TelemetryConfig carrying both Langfuse key references.
func langfuseRefs() TelemetryConfig {
	return TelemetryConfig{
		LangfusePublicKeyRef: "secret://LANGFUSE_PUBLIC_KEY",
		LangfuseSecretKeyRef: "secret://LANGFUSE_SECRET_KEY",
	}
}

// TestTelemetry_Validate_AcceptsSupportedDestinations: each supported destination
// validates for a deep-research run and a worker run alike.
func TestTelemetry_Validate_AcceptsSupportedDestinations(t *testing.T) {
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
			for _, base := range []ResearchConfig{Default(), validWorker()} {
				t.Run(base.Agent, func(t *testing.T) {
					must := require.New(t)
					base.Telemetry = tt.tc
					must.NoError(base.Validate())
				})
			}
		})
	}
}

// TestTelemetry_Validate_RejectsBadDestinations: every rejection names the offending field
// and never echoes the value, which may be a pasted credential.
func TestTelemetry_Validate_RejectsBadDestinations(t *testing.T) {
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
		{"otlp empty hostname", func(tc *TelemetryConfig) {
			tc.OTLPEndpoint = "https://:443/otel"
		}, "telemetry.otlp_endpoint"},
		{"otlp bare trailing question mark", func(tc *TelemetryConfig) {
			tc.OTLPEndpoint = "https://otel.example/?"
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
			for _, base := range []ResearchConfig{Default(), validWorker()} {
				t.Run(base.Agent, func(t *testing.T) {
					must := require.New(t)
					tt.mutate(&base.Telemetry)
					err := base.Validate()
					must.Error(err, "validation must reject the config")
					must.ErrorContains(err, tt.want)
					must.NotContains(err.Error(), literal, "validation error leaked the literal")
				})
			}
		})
	}
}

// TestTelemetry_LangfuseTarget_DefaultsOnlyWithKeys: the default endpoint applies only when both key
// references are set and no endpoint is given.
func TestTelemetry_LangfuseTarget_DefaultsOnlyWithKeys(t *testing.T) {
	must := require.New(t)
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
			must := require.New(t)
			must.Equal(tt.want, tt.tc.LangfuseTarget())
		})
	}
	must.Zero(Default().Telemetry, "Default() must leave telemetry unset")
}

// TestTelemetry_ApplyFlags_SetsOnlyItsOwnField: set alone, each telemetry flag may change
// only its own field from Default().
func TestTelemetry_ApplyFlags_SetsOnlyItsOwnField(t *testing.T) {
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
			must := require.New(t)
			cfg := Default()
			fs := newFlagSet(t)
			must.NoError(fs.Parse(tt.args), "fs.Parse")
			must.NoError(ApplyFlags(&cfg, fs), "ApplyFlags")
			must.Equal(tt.want, cfg.Telemetry)
			rest := cfg
			rest.Telemetry = TelemetryConfig{}
			must.Equal(Default(), rest, "an unrelated field changed")
		})
	}
}

// TestTelemetry_ApplyFlags_LayersOverBaseBlock: a base config's telemetry block
// survives unset flags, and a set flag overrides only its own field.
func TestTelemetry_ApplyFlags_LayersOverBaseBlock(t *testing.T) {
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
			must := require.New(t)
			cfg, err := DecodeBase(strings.NewReader(base))
			must.NoError(err, "DecodeBase")
			fs := newFlagSet(t)
			must.NoError(fs.Parse(tt.args), "fs.Parse")
			must.NoError(ApplyFlags(&cfg, fs), "ApplyFlags")
			must.Equal(tt.want, cfg.Telemetry)
		})
	}
}

// TestTelemetry_EncodeDecode_RoundTrip: the telemetry block survives EncodeJSON -> DecodeBase
// under its documented keys, and an unset block is omitted.
func TestTelemetry_EncodeDecode_RoundTrip(t *testing.T) {
	must := require.New(t)
	var empty bytes.Buffer
	must.NoError(Default().EncodeJSON(&empty), "EncodeJSON")
	must.NotContains(empty.String(), `"telemetry"`, "an unset telemetry block is encoded")

	in := Default()
	in.Telemetry = langfuseRefs()
	in.Telemetry.LangfuseEndpoint = "https://langfuse.internal/api/public/otel"
	var buf bytes.Buffer
	must.NoError(in.EncodeJSON(&buf), "EncodeJSON")
	for _, key := range []string{"telemetry", "langfuse_endpoint", "langfuse_public_key_ref", "langfuse_secret_key_ref"} {
		t.Run(key, func(t *testing.T) {
			must := require.New(t)
			must.Contains(buf.String(), `"`+key+`"`, "encoded config lacks the key")
		})
	}
	out, err := DecodeBase(&buf)
	must.NoError(err, "DecodeBase")
	must.Equal(in.Telemetry, out.Telemetry, "round trip lost data")

	otlp, err := DecodeBase(strings.NewReader("telemetry:\n  otlp_endpoint: https://otel.example\n"))
	must.NoError(err, "DecodeBase YAML")
	must.Equal("https://otel.example", otlp.Telemetry.OTLPEndpoint)
}
