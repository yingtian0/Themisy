package themisy.authorization

import rego.v1

deny contains "MISSING_SUBJECT" if {
	input.user_id == ""
}

deny contains "MISSING_AGENT" if {
	input.agent_id == ""
}

valid_subject_type if {
	input.subject_type == "user"
}

valid_subject_type if {
	input.subject_type == "ci"
}

deny contains "INVALID_SUBJECT_TYPE" if {
	input.environment == "production"
	not valid_subject_type
}

deny contains "MISSING_DELEGATION" if {
	input.environment == "production"
	input.delegation_ref == ""
}

deny contains "UNSUPPORTED_POLICY_INPUT" if {
	input.version != "themisy.policy.input/v1alpha1"
}

deny contains "MISSING_PINNED_CONTEXT" if {
	input.environment == "production"
	hashes := [
		object.get(input, "plan_hash", ""),
		object.get(input, "contract_hash", ""),
		object.get(input, "profile_hash", ""),
		object.get(input, "policy_hash", ""),
		object.get(input, "evidence_hash", ""),
	]
	some value in hashes
	not startswith(value, "sha256:")
}

approval_required contains "MISSING_APPROVAL_PROOF" if {
	input.risk in {"high", "critical"}
	count(object.get(input, "approval_proofs", [])) == 0
}

default allow := false

allow if {
	count(deny) == 0
	count(approval_required) == 0
}

default require_approval := false

require_approval if {
	count(deny) == 0
	count(approval_required) > 0
}

decision := {
	"allow": allow,
	"require_approval": require_approval,
	"reasons": sort(array.concat([reason | some reason in deny], [reason | some reason in approval_required])),
	"required_roles": ["service-owner"],
}
