package main

import "testing"

func TestParseResourcePlaneHealthSecretARNMap(t *testing.T) {
	valid := `{"rp-tokyo-01":"arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/rp-tokyo-health-AbCdEf"}`
	parsed, err := parseResourcePlaneHealthSecretARNMap(valid)
	if err != nil || parsed["rp-tokyo-01"] == "" || len(parsed) != 1 {
		t.Fatalf("valid health Secret ARN map = %#v, %v", parsed, err)
	}
	empty, err := parseResourcePlaneHealthSecretARNMap(`{}`)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty health Secret ARN map should disable ingress: %#v, %v", empty, err)
	}
	for name, raw := range map[string]string{
		"malformed json":    `{`,
		"invalid plane id":  `{"Tokyo":"arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/health-AbCdEf"}`,
		"invalid arn":       `{"rp-tokyo-01":"not-a-secret-arn"}`,
		"shared secret ARN": `{"rp-one":"arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/health-AbCdEf","rp-two":"arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/health-AbCdEf"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseResourcePlaneHealthSecretARNMap(raw); err == nil {
				t.Fatalf("invalid health Secret map accepted: %s", raw)
			}
		})
	}
}
