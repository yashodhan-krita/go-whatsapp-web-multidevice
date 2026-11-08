package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestOfflineBuilderDoesNotDependOnWhatsApp(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-tags", "purego", ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "go.mau.fi/whatsmeow") || strings.Contains(line, "infrastructure/whatsapp") {
			t.Fatalf("offline helper depends on WhatsApp: %s", line)
		}
		if strings.Contains(line, "go-whatsapp-web-multidevice/cmd") && !strings.Contains(line, "dbreuse-offline") {
			t.Fatalf("offline helper depends on the server command package: %s", line)
		}
	}
}
