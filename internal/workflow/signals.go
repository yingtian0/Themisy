package workflow

const (
	SignalApproval = "release.approval"
	SignalPause    = "release.pause"
	SignalResume   = "release.resume"
	SignalCancel   = "release.cancel"
	QueryState     = "release.state"
)

type ApprovalSignal struct {
	Issuer     string   `json:"issuer,omitempty"`
	ApprovalID string   `json:"approval_id"`
	Actor      string   `json:"actor"`
	Roles      []string `json:"roles"`
	Approve    bool     `json:"approve"`
	Action     string   `json:"action,omitempty"`
}

type ControlSignal struct {
	Actor  string `json:"actor"`
	Reason string `json:"reason,omitempty"`
}
