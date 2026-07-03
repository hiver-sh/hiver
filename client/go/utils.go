package client

import "strings"

// AllowSandbox builds egress rules that let an agent create and reach a single
// nested sandbox named sandboxKey through the gateway, using a fixed config the
// agent cannot tamper with.
//
// The agent may POST to create the sandbox by key, but its request body is
// replaced with config (BodyStrategyReplace) so it cannot influence what gets
// created; a passthrough rule allows the sandbox's gateway proxy routes. Both
// the Docker ("gateway") and k8s ("gateway.hiver") gateway hosts are covered.
//
// Pass allowedDirs to also open the nested sandbox's file API under specific
// directories. Each entry allows POST/GET/DELETE on the file endpoint's
// ".../file/<dir>/**" glob, so the agent can seed and read back files there
// without reaching the rest of the sandbox's filesystem. When none are given,
// no file rules are added. Entries are matched relative to the file endpoint; a
// leading slash is stripped, so both "workspace/inputs" and "/workspace/inputs"
// work.
//
// Add the returned rules to SandboxConfig.Egress.
func AllowSandbox(sandboxKey string, config SandboxConfig, allowedDirs ...string) []EgressRule {
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
		if len(allowedDirs) > 0 {
			paths := make([]string, len(allowedDirs))
			for i, dir := range allowedDirs {
				paths[i] = "/sandbox/*/v1/" + sandboxKey + "/file/" + strings.TrimPrefix(dir, "/") + "/**"
			}
			rules = append(rules, EgressRule{
				Access: "allow",
				Host:   host,
				Paths:  paths,
			})
		}
	}
	return rules
}
