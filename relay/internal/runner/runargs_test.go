package runner

import (
	"context"
	"strings"
	"testing"
)

func TestRunArgsAgentVolumeAndBinary(t *testing.T) {
	d := NewDockerDriver()
	d.AgentBinary = "/opt/tunr/tunr-linux"
	dep := DeploySpec{App: AppSpec{ID: "a_1", Name: "bot", Kind: "agent", VolumeMB: 512,
		Env: map[string]string{"TUNR_AGENT_BOOTSTRAP": "x"}}, DeploymentID: "dep_1"}
	args := strings.Join(d.runArgs(context.Background(), dep, "tunr-app-a_1", d.Tier, 1, 8080), " ")
	for _, want := range []string{
		"--read-only", "--security-opt no-new-privileges", "--runtime runsc",
		"--mount type=volume,src=tunr-vol-a_1,dst=/data",
		"--mount type=bind,src=/opt/tunr/tunr-linux,dst=/opt/tunr/bin/tunr,readonly",
		"-e TUNR_HOSTED_AGENT=1", "-e TUNR_DATA_DIR=/data",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in %s", want, args)
		}
	}

	web := DeploySpec{App: AppSpec{ID: "a_2", Name: "site"}}
	args = strings.Join(d.runArgs(context.Background(), web, "tunr-app-a_2", d.Tier, 1, 8080), " ")
	if strings.Contains(args, "/data") || strings.Contains(args, "/opt/tunr/bin/tunr") || strings.Contains(args, "TUNR_HOSTED_AGENT") {
		t.Errorf("web app got agent mounts: %s", args)
	}
}
