package provider

type ReasoningEffortLevel string

// none, minimal, low, medium, high, xhigh, max
const (
	ReasoningEffortLevelNone    ReasoningEffortLevel = "none"
	ReasoningEffortLevelMinimal ReasoningEffortLevel = "minimal"
	ReasoningEffortLevelLow     ReasoningEffortLevel = "low"
	ReasoningEffortLevelMedium  ReasoningEffortLevel = "medium"
	ReasoningEffortLevelHigh    ReasoningEffortLevel = "high"
	ReasoningEffortLevelXHigh   ReasoningEffortLevel = "xhigh"
	ReasoningEffortLevelMax     ReasoningEffortLevel = "max"
)
