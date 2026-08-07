package fpkgen

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallLocalUsesInstallVolumeLabelFirst(t *testing.T) {
	installer, appDir, logPath := newFakeAppcenterCLI(t, "2", "0")

	if err := installer.InstallLocal(appDir, map[string]string{"watchcow.install_volume": "3"}); err != nil {
		t.Fatalf("InstallLocal() error = %v", err)
	}

	got := readCommandLog(t, logPath)
	want := []string{
		"install-local --volume 3",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestInstallLocalUsesConfiguredDefaultVolume(t *testing.T) {
	installer, appDir, logPath := newFakeAppcenterCLI(t, "2", "0")

	if err := installer.InstallLocal(appDir, nil); err != nil {
		t.Fatalf("InstallLocal() error = %v", err)
	}

	got := readCommandLog(t, logPath)
	want := []string{
		"default-volume",
		"install-local --volume 2",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestInstallLocalFallsBackToVolumeOneWithoutDefaultVolume(t *testing.T) {
	installer, appDir, logPath := newFakeAppcenterCLI(t, "0", "0")

	if err := installer.InstallLocal(appDir, nil); err != nil {
		t.Fatalf("InstallLocal() error = %v", err)
	}

	got := readCommandLog(t, logPath)
	want := []string{
		"default-volume",
		"install-local --volume 1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestStartAppSkipsStartWhenAppcenterStatusIsStarting(t *testing.T) {
	installer, _, logPath := newFakeAppcenterCLI(t, "2", "0")
	t.Setenv("APP_STATUS_OUTPUT", "starting\n")

	if err := installer.StartApp("watchcow.xunlei"); err != nil {
		t.Fatalf("StartApp() error = %v", err)
	}

	got := readCommandLog(t, logPath)
	want := []string{
		"status watchcow.xunlei",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestStartAppRunsStartWhenAppcenterStatusIsNotStarting(t *testing.T) {
	installer, _, logPath := newFakeAppcenterCLI(t, "2", "0")
	t.Setenv("APP_STATUS_OUTPUT", "running\n")

	if err := installer.StartApp("watchcow.xunlei"); err != nil {
		t.Fatalf("StartApp() error = %v", err)
	}

	got := readCommandLog(t, logPath)
	want := []string{
		"status watchcow.xunlei",
		"start watchcow.xunlei",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestStartAppRunsStartWhenAppcenterStatusFails(t *testing.T) {
	installer, _, logPath := newFakeAppcenterCLI(t, "2", "0")
	t.Setenv("APP_STATUS_EXIT", "1")

	if err := installer.StartApp("watchcow.xunlei"); err != nil {
		t.Fatalf("StartApp() error = %v", err)
	}

	got := readCommandLog(t, logPath)
	want := []string{
		"status watchcow.xunlei",
		"start watchcow.xunlei",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestUninstallPropagatesCLIError(t *testing.T) {
	installer, _, logPath := newFakeAppcenterCLI(t, "2", "0")
	t.Setenv("UNINSTALL_EXIT", "1")

	err := installer.Uninstall("watchcow.test")
	if err == nil || !strings.Contains(err.Error(), "appcenter-cli uninstall watchcow.test failed") {
		t.Fatalf("Uninstall() error = %v, want CLI failure", err)
	}

	got := readCommandLog(t, logPath)
	want := []string{"stop watchcow.test", "uninstall watchcow.test", "start watchcow.test"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestUninstallVerifiesAppWasRemoved(t *testing.T) {
	installer, _, logPath := newFakeAppcenterCLI(t, "2", "0")

	if err := installer.Uninstall("watchcow.test"); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	got := readCommandLog(t, logPath)
	want := []string{"stop watchcow.test", "uninstall watchcow.test", "list"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestUninstallRejectsFalseSuccess(t *testing.T) {
	installer, _, _ := newFakeAppcenterCLI(t, "2", "0")
	t.Setenv("APP_LIST_OUTPUT", "│ watchcow.test │ running │\n")

	err := installer.Uninstall("watchcow.test")
	if err == nil || !strings.Contains(err.Error(), "is still installed") {
		t.Fatalf("Uninstall() error = %v, want failed verification", err)
	}
}

func TestUninstallReportsFailedRestore(t *testing.T) {
	installer, _, _ := newFakeAppcenterCLI(t, "2", "0")
	t.Setenv("UNINSTALL_EXIT", "1")
	t.Setenv("START_EXIT", "1")

	err := installer.Uninstall("watchcow.test")
	var restoreErr *RestoreStoppedAppError
	if !errors.As(err, &restoreErr) || restoreErr.RestoreErr == nil {
		t.Fatalf("Uninstall() error = %v, want RestoreStoppedAppError", err)
	}
}

func TestParseInstallVolume(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{name: "volume index", output: "1\n", want: "1"},
		{name: "padded volume index", output: "03\n", want: "3"},
		{name: "zero volume index", output: "0\n", want: ""},
		{name: "empty", output: "\n", want: ""},
		{name: "english missing", output: "[Error]Use `--volume` to specify the volume index", want: ""},
		{name: "chinese missing", output: "未设置默认安装卷", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseInstallVolume(tt.output); got != tt.want {
				t.Fatalf("parseInstallVolume(%q) = %q, want %q", tt.output, got, tt.want)
			}
		})
	}
}

func newFakeAppcenterCLI(t *testing.T, defaultVolumeOutput string, defaultVolumeExit string) (*Installer, string, string) {
	t.Helper()

	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	if err := os.Mkdir(appDir, 0755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scriptPath := filepath.Join(dir, "appcenter-cli")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$COMMAND_LOG"
if [ "$1" = "default-volume" ]; then
  printf '%s\n' "$DEFAULT_VOLUME_OUTPUT"
  exit "$DEFAULT_VOLUME_EXIT"
fi
if [ "$1" = "install-local" ]; then
  exit 0
fi
if [ "$1" = "status" ]; then
  printf '%s\n' "$APP_STATUS_OUTPUT"
  exit "$APP_STATUS_EXIT"
fi
if [ "$1" = "start" ]; then
  exit "$START_EXIT"
fi
if [ "$1" = "stop" ]; then
  exit 0
fi
if [ "$1" = "uninstall" ]; then
  exit "$UNINSTALL_EXIT"
fi
if [ "$1" = "list" ]; then
  printf '%s' "$APP_LIST_OUTPUT"
  exit "$APP_LIST_EXIT"
fi
exit 1
`

	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	t.Setenv("COMMAND_LOG", logPath)
	t.Setenv("DEFAULT_VOLUME_OUTPUT", defaultVolumeOutput)
	t.Setenv("DEFAULT_VOLUME_EXIT", defaultVolumeExit)
	t.Setenv("APP_STATUS_OUTPUT", "running")
	t.Setenv("APP_STATUS_EXIT", "0")
	t.Setenv("START_EXIT", "0")
	t.Setenv("UNINSTALL_EXIT", "0")
	t.Setenv("APP_LIST_OUTPUT", "")
	t.Setenv("APP_LIST_EXIT", "0")

	return &Installer{appcenterCLIPath: scriptPath}, appDir, logPath
}

func readCommandLog(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	return strings.Split(strings.TrimSpace(string(data)), "\n")
}
