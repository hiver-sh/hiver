package client

// AllowSandbox builds egress rules that let an agent create and reach a single
// nested sandbox named sandboxKey through the gateway, using a fixed config the
// agent cannot tamper with.
//
// The agent may POST to create the sandbox by key, but its request body is
// replaced with config (BodyStrategyReplace) so it cannot influence what gets
// created; a passthrough rule allows the sandbox's gateway proxy routes. Both
// the Docker ("gateway") and k8s ("gateway.hiver") gateway hosts are covered.
// Add the returned rules to SandboxConfig.Egress.
func AllowSandbox(sandboxKey string, config SandboxConfig) []EgressRule {
	var rules []EgressRule
	for _, host := range []string{"gateway", "gateway.hiver"} {
		rules = append(rules,
			EgressRule{
				Access:  "allow",
				Host:    host,
				Paths:   []string{"/v1/sandboxes/" + sandboxKey},
				Methods: []string{"POST"},
				Override: &EgressOverride{
					Body:         config,
					BodyStrategy: BodyStrategyReplace,
				},
			},
			EgressRule{
				Access: "allow",
				Host:   host,
				Paths:  []string{"/sandbox/*/v1/" + sandboxKey + "/proxy/**"},
			},
		)
	}
	return rules
}
