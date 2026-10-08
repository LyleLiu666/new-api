package types

// UsageFact records a quantity's provenance, not a price or user intent. An
// unknown quantity is nil; a reported zero remains an explicit pointer to zero.
type UsageFact struct {
	Field      string           `json:"field"`
	Unit       string           `json:"unit"`
	Quantity   *float64         `json:"quantity"`
	Source     string           `json:"source"`
	Algorithm  string           `json:"algorithm,omitempty"`
	Partial    bool             `json:"partial,omitempty"`
	Estimation *UsageEstimation `json:"estimation,omitempty"`
}

// UsageEstimation identifies the counter actually used. It contains only
// counter configuration and numeric observations, never the counted content.
// Quantity is the local estimate before a provider's partial lower bound.
type UsageEstimation struct {
	Version           string                   `json:"version"`
	Model             string                   `json:"model"`
	Method            string                   `json:"method"`
	Tokenizer         string                   `json:"tokenizer,omitempty"`
	DependencyVersion string                   `json:"dependency_version,omitempty"`
	Parameters        map[string]float64       `json:"parameters,omitempty"`
	Settings          map[string]bool          `json:"settings,omitempty"`
	Quantity          float64                  `json:"quantity"`
	Components        []UsageEstimateComponent `json:"components,omitempty"`
	OmittedComponents int                      `json:"omitted_components,omitempty"`
}

// A media counter component contains only the observed numeric parameters,
// never file identifiers, URLs, media payloads or user-supplied descriptions.
type UsageEstimateComponent struct {
	Index      int                `json:"index"`
	Kind       string             `json:"kind"`
	Method     string             `json:"method"`
	Quantity   float64            `json:"quantity"`
	Parameters map[string]float64 `json:"parameters,omitempty"`
	Settings   map[string]bool    `json:"settings,omitempty"`
}
