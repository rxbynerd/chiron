package config

import "errors"

// DefaultLangfuseEndpoint is the Langfuse Cloud OTLP base URL, used when
// both Langfuse key references are set and no endpoint is given.
const DefaultLangfuseEndpoint = "https://cloud.langfuse.com/api/public/otel"

// TelemetryConfig names where a run's spans are forwarded: a generic OTLP
// collector or Langfuse, never both. With every field empty the standard
// OTEL_EXPORTER_OTLP_* environment variables still apply. Endpoints and key
// references are validated like the fleet pairs: spans carry the research
// query, and the Langfuse keys travel to the Langfuse endpoint.
type TelemetryConfig struct {
	// OTLPEndpoint is an OTLP/HTTP collector base URL; spans go to its
	// /v1/traces path. Absolute https://, http:// for loopback only.
	OTLPEndpoint string `json:"otlp_endpoint,omitempty" yaml:"otlp_endpoint,omitempty"`
	// LangfuseEndpoint is the Langfuse OTLP base URL, validated like
	// OTLPEndpoint; DefaultLangfuseEndpoint applies when it is empty.
	LangfuseEndpoint string `json:"langfuse_endpoint,omitempty" yaml:"langfuse_endpoint,omitempty"`
	// LangfusePublicKeyRef and LangfuseSecretKeyRef are secret:// references
	// to the Langfuse project keys — never literals. Both or neither.
	LangfusePublicKeyRef string `json:"langfuse_public_key_ref,omitempty" yaml:"langfuse_public_key_ref,omitempty"`
	LangfuseSecretKeyRef string `json:"langfuse_secret_key_ref,omitempty" yaml:"langfuse_secret_key_ref,omitempty"`
}

// LangfuseTarget returns the Langfuse OTLP base URL spans are forwarded to:
// LangfuseEndpoint, or DefaultLangfuseEndpoint when only the keys are set.
// It is empty unless both key references are set.
func (t TelemetryConfig) LangfuseTarget() string {
	if t.LangfusePublicKeyRef == "" || t.LangfuseSecretKeyRef == "" {
		return ""
	}
	if t.LangfuseEndpoint != "" {
		return t.LangfuseEndpoint
	}
	return DefaultLangfuseEndpoint
}

// validate checks the telemetry fields for every agent. Errors name the
// field and never echo a value.
func (t TelemetryConfig) validate() error {
	if err := validEndpoint("telemetry.otlp_endpoint", t.OTLPEndpoint); err != nil {
		return err
	}
	if err := validEndpoint("telemetry.langfuse_endpoint", t.LangfuseEndpoint); err != nil {
		return err
	}
	if err := validKeyRef("telemetry.langfuse_public_key_ref", t.LangfusePublicKeyRef); err != nil {
		return err
	}
	if err := validKeyRef("telemetry.langfuse_secret_key_ref", t.LangfuseSecretKeyRef); err != nil {
		return err
	}
	switch {
	case t.LangfusePublicKeyRef != "" && t.LangfuseSecretKeyRef == "":
		return errors.New("telemetry.langfuse_secret_key_ref: is required with telemetry.langfuse_public_key_ref")
	case t.LangfuseSecretKeyRef != "" && t.LangfusePublicKeyRef == "":
		return errors.New("telemetry.langfuse_public_key_ref: is required with telemetry.langfuse_secret_key_ref")
	}
	langfuse := t.LangfusePublicKeyRef != ""
	if t.LangfuseEndpoint != "" && !langfuse {
		return errors.New("telemetry.langfuse_endpoint: needs telemetry.langfuse_public_key_ref and telemetry.langfuse_secret_key_ref; Langfuse ingestion requires authentication")
	}
	if t.OTLPEndpoint != "" && langfuse {
		return errors.New("telemetry.otlp_endpoint: conflicts with the Langfuse keys; a run forwards spans to one destination")
	}
	return nil
}
