package fpkgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"

	"watchcow/internal/app"
)

func TestExtractConfigRejectsUnsafeExplicitAppName(t *testing.T) {
	generator := &Generator{}
	container := &dockercontainer.InspectResponse{
		ContainerJSONBase: &dockercontainer.ContainerJSONBase{Name: "/test"},
		Config: &dockercontainer.Config{
			Labels: map[string]string{"watchcow.appname": "unsafe/name"},
		},
	}

	_, err := generator.extractConfig(container)
	if err == nil || !strings.Contains(err.Error(), "invalid watchcow.appname") {
		t.Fatalf("extractConfig() error = %v, want explicit appname validation error", err)
	}
}

func TestExtractConfigEnforcesExplicitAppNameLength(t *testing.T) {
	newContainer := func(appName string) *dockercontainer.InspectResponse {
		return &dockercontainer.InspectResponse{
			ContainerJSONBase: &dockercontainer.ContainerJSONBase{
				ID: "1234567890abcdef", Name: "/test",
				HostConfig: &dockercontainer.HostConfig{},
			},
			Config: &dockercontainer.Config{Labels: map[string]string{"watchcow.appname": appName}},
		}
	}

	generator := &Generator{}
	valid := strings.Repeat("a", app.MaxAppNameLength)
	config, err := generator.extractConfig(newContainer(valid))
	if err != nil || config.AppName != valid {
		t.Fatalf("maximum-length explicit appname was not preserved: config=%+v err=%v", config, err)
	}
	for _, invalid := range []string{
		strings.Repeat("a", app.MinAppNameLength-1),
		strings.Repeat("a", app.MaxAppNameLength+1),
	} {
		if _, err := generator.extractConfig(newContainer(invalid)); err == nil || !strings.Contains(err.Error(), "invalid watchcow.appname") {
			t.Fatalf("explicit appname %q error = %v, want length error", invalid, err)
		}
	}
}

func TestExtractConfigBoundsIssue39DefaultLabelAppName(t *testing.T) {
	generator := &Generator{}
	container := &dockercontainer.InspectResponse{
		ContainerJSONBase: &dockercontainer.ContainerJSONBase{
			ID: "1234567890abcdef", Name: "/beecount-beecount-cloud-1",
			HostConfig: &dockercontainer.HostConfig{},
		},
		Config: &dockercontainer.Config{Labels: map[string]string{"watchcow.enable": "true"}},
	}

	config, err := generator.extractConfig(container)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.AppName) > app.MaxAppNameLength {
		t.Fatalf("default label appname exceeds fnOS limit: %q", config.AppName)
	}
}

func TestExtractConfigSupportsHostNetworkWithExplicitServicePort(t *testing.T) {
	generator := &Generator{}
	container := &dockercontainer.InspectResponse{
		ContainerJSONBase: &dockercontainer.ContainerJSONBase{
			ID: "1234567890abcdef", Name: "/host-app",
			HostConfig: &dockercontainer.HostConfig{NetworkMode: "host"},
		},
		Config: &dockercontainer.Config{
			Image: "example/host-app:latest",
			Labels: map[string]string{
				"watchcow.enable":       "true",
				"watchcow.service_port": "8080",
			},
		},
	}

	config, err := generator.extractConfig(container)
	if err != nil {
		t.Fatal(err)
	}
	entry := config.GetDefaultEntry()
	if config.Port != "8080" || entry == nil || entry.Port != "8080" {
		t.Fatalf("host-network service port was not propagated: config=%+v entry=%+v", config, entry)
	}
	if data := NewTemplateData(config); data.Port != "8080" {
		t.Fatalf("manifest service port = %q, want 8080", data.Port)
	}
}

func TestGenerateFromConfigRejectsUnsafeAppNameBeforeEditingOutput(t *testing.T) {
	appDir := t.TempDir()
	marker := filepath.Join(appDir, "keep")
	if err := os.WriteFile(marker, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}

	generator := &Generator{}
	err := generator.GenerateFromConfig(&AppConfig{AppName: "unsafe/name"}, appDir)
	if err == nil || !strings.Contains(err.Error(), "invalid appname") {
		t.Fatalf("GenerateFromConfig() error = %v, want appname validation error", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("invalid config modified output directory: %v", err)
	}
}

func TestValidateAppNameRejectsOverlongIdentifier(t *testing.T) {
	err := ValidateAppName(strings.Repeat("a", app.MaxAppNameLength+1))
	if err == nil || !strings.Contains(err.Error(), "at most 32") {
		t.Fatalf("ValidateAppName() error = %v, want length error", err)
	}
}

func TestValidateAppNameEnforcesFnpackLengthBounds(t *testing.T) {
	if err := ValidateAppName(strings.Repeat("a", app.MinAppNameLength)); err != nil {
		t.Fatalf("minimum-length appname rejected: %v", err)
	}
	if err := ValidateAppName(strings.Repeat("a", app.MaxAppNameLength)); err != nil {
		t.Fatalf("maximum-length appname rejected: %v", err)
	}
	if err := ValidateAppName(strings.Repeat("a", app.MinAppNameLength-1)); err == nil || !strings.Contains(err.Error(), "at least 3") {
		t.Fatalf("short appname error = %v, want minimum-length error", err)
	}
}

func TestExtractFirstPortUsesStableContainerPortOrder(t *testing.T) {
	container := &dockercontainer.InspectResponse{
		ContainerJSONBase: &dockercontainer.ContainerJSONBase{
			HostConfig: &dockercontainer.HostConfig{
				PortBindings: nat.PortMap{
					nat.Port("10000/tcp"): {{HostPort: "110000"}},
					nat.Port("3000/tcp"):  {{HostPort: "13000"}},
					nat.Port("53/udp"):    {{HostPort: "2053"}},
				},
			},
		},
	}

	if got := extractFirstPort(container); got != "13000" {
		t.Errorf("extractFirstPort() = %q, want %q", got, "13000")
	}
}

func TestExtractFirstPortIgnoresUDPOnlyBindings(t *testing.T) {
	container := &dockercontainer.InspectResponse{
		ContainerJSONBase: &dockercontainer.ContainerJSONBase{
			HostConfig: &dockercontainer.HostConfig{
				PortBindings: nat.PortMap{
					nat.Port("53/udp"): {{HostPort: "2053"}},
				},
			},
		},
	}

	if got := extractFirstPort(container); got != "" {
		t.Errorf("extractFirstPort() = %q, want no HTTP service port for UDP-only bindings", got)
	}
}

func TestExtractFirstPortChoosesBindingDeterministically(t *testing.T) {
	container := &dockercontainer.InspectResponse{
		ContainerJSONBase: &dockercontainer.ContainerJSONBase{
			HostConfig: &dockercontainer.HostConfig{
				PortBindings: nat.PortMap{
					nat.Port("8080/tcp"): {
						{HostIP: "::", HostPort: "28080"},
						{HostIP: "0.0.0.0", HostPort: "18080"},
					},
				},
			},
		},
	}

	if got := extractFirstPort(container); got != "18080" {
		t.Errorf("extractFirstPort() = %q, want deterministic lowest host port", got)
	}
}
