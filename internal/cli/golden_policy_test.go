package cli

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoldenImagePolicyIsExplicitAndConservative(t *testing.T) {
	required := []string{
		"ssh.service", "ssh.socket", "serial-getty@ttyS0.service",
		"snapd.service", "snapd.socket", "snapd.seeded.service",
		"ModemManager.service", "udisks2.service", "polkit.service",
		"apport.service", "apport-autoreport.service", "apport-autoreport.timer",
		"unattended-upgrades.service", "apt-daily.service", "apt-daily.timer",
		"apt-daily-upgrade.service", "apt-daily-upgrade.timer",
		"lxd-installer.socket", "fwupd.service", "fwupd-refresh.service", "fwupd-refresh.timer",
	}
	for _, unit := range required {
		if !strings.Contains(imageBuildScript, "'"+unit+"'") {
			t.Fatalf("required image mask missing: %s", unit)
		}
	}
	for _, requiredText := range []string{
		"systemctl set-default multi-user.target",
		"/etc/runnerloom-image-policy.json",
		"runnerloom/image-policy/v1",
		"imagePolicySHA256",
	} {
		if !strings.Contains(imageBuildScript, requiredText) {
			t.Fatalf("Golden Image policy evidence missing %q", requiredText)
		}
	}
	for _, destructive := range []string{"apt-get purge", "apt-get autoremove", "dpkg --purge"} {
		if strings.Contains(imageBuildScript, destructive) {
			t.Fatalf("Golden Image optimization must not remove packages speculatively: %q", destructive)
		}
	}
}

func TestGoldenImagePolicyQualificationScriptParses(t *testing.T) {
	path := filepath.Join("..", "..", "scripts", "ci-golden-image-policy.sh")
	if output, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("invalid Golden Image policy qualification script: %v: %s", err, output)
	}
}
