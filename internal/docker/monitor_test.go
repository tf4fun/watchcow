package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"

	"watchcow/internal/app"
	"watchcow/internal/fpkgen"
)

type mapConfigProvider map[string]*StoredConfig

func (p mapConfigProvider) GetByKey(key string) *StoredConfig {
	return p[key]
}

type fakeAppInstaller struct {
	uninstallErr    error
	installErr      error
	startErr        error
	stopErr         error
	uninstallCalls  []string
	installCalls    int
	startCalls      []string
	stopCalls       []string
	installedChecks []string
	installed       bool
	installedApps   map[string]bool
	onUninstall     func()
}

func (i *fakeAppInstaller) InstallLocal(string, map[string]string) error {
	i.installCalls++
	return i.installErr
}
func (i *fakeAppInstaller) StartApp(appName string) error {
	i.startCalls = append(i.startCalls, appName)
	return i.startErr
}
func (i *fakeAppInstaller) StopApp(appName string) error {
	i.stopCalls = append(i.stopCalls, appName)
	return i.stopErr
}
func (i *fakeAppInstaller) IsAppInstalled(appName string) bool {
	i.installedChecks = append(i.installedChecks, appName)
	if i.installedApps != nil {
		return i.installedApps[appName]
	}
	return i.installed
}
func (i *fakeAppInstaller) Uninstall(appName string) error {
	i.uninstallCalls = append(i.uninstallCalls, appName)
	if i.onUninstall != nil {
		i.onUninstall()
	}
	if i.uninstallErr == nil && i.installedApps != nil {
		i.installedApps[appName] = false
	}
	return i.uninstallErr
}

func (p mapConfigProvider) GetCompatibleCandidates(image string, identityPorts map[string]string, portOptions map[string][]string) []StoredConfigMatch {
	var matches []StoredConfigMatch
	for key, config := range p {
		if MatchesCompatibleContainerKey(key, image, identityPorts, portOptions) {
			matches = append(matches, StoredConfigMatch{Key: key, Config: config})
		}
	}
	return matches
}

func (p mapConfigProvider) GetNamedCandidates(containerName string) []StoredConfigMatch {
	var matches []StoredConfigMatch
	for key, config := range p {
		_, identity, ok := strings.Cut(key, "|")
		if !ok || !strings.HasPrefix(identity, "@") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(identity, "@"), ";")
		if name == strings.TrimPrefix(containerName, "/") {
			matches = append(matches, StoredConfigMatch{Key: key, Config: config})
		}
	}
	return matches
}

func (p mapConfigProvider) GetDeletingConfigs() []StoredConfigMatch {
	var matches []StoredConfigMatch
	for key, config := range p {
		if config.Deleting {
			matches = append(matches, StoredConfigMatch{Key: key, Config: config})
		}
	}
	return matches
}

func (p mapConfigProvider) GetAllConfigs() []StoredConfigMatch {
	var matches []StoredConfigMatch
	for key, config := range p {
		matches = append(matches, StoredConfigMatch{Key: key, Config: config})
	}
	return matches
}

func (p mapConfigProvider) MarkApplied(key, revision string) error {
	if config := p[key]; config != nil && config.Revision == revision {
		config.Pending = false
		config.LastError = ""
	}
	return nil
}

func (p mapConfigProvider) MarkFailed(key, revision, message string) error {
	if config := p[key]; config != nil && config.Revision == revision {
		config.LastError = message
		if !config.Deleting {
			config.Pending = true
		}
	}
	return nil
}

func (p mapConfigProvider) CompleteDelete(appName string) error {
	for key, config := range p {
		if config.AppName == appName && config.Deleting {
			delete(p, key)
		}
	}
	return nil
}

func (p mapConfigProvider) MigrateLegacy(oldKey, newKey string) (*StoredConfig, error) {
	config := p[oldKey]
	if config == nil {
		return p[newKey], nil
	}
	delete(p, oldKey)
	config.Key = newKey
	p[newKey] = config
	return config, nil
}

func TestMonitorStopIsIdempotent(t *testing.T) {
	monitor := &Monitor{
		stopCh: make(chan struct{}),
	}

	monitor.Stop()
	monitor.Stop()
}

func TestGetAppNameFromLabelsUsesSanitizedDefault(t *testing.T) {
	got := getAppNameFromLabels(map[string]string{}, "my_app")
	want := "watchcow.my-app"
	if got != want {
		t.Errorf("getAppNameFromLabels() = %q, want %q", got, want)
	}
}

func TestGetAppNameFromLabelsPreservesExplicitLabel(t *testing.T) {
	got := getAppNameFromLabels(map[string]string{
		"watchcow.appname": "custom.app",
	}, "my_app")
	want := "custom.app"
	if got != want {
		t.Errorf("getAppNameFromLabels() = %q, want %q", got, want)
	}
}

func TestRegisterAppFromLabelsWithRedirectOnlyRestoresDefaultEntry(t *testing.T) {
	monitor := &Monitor{registry: app.NewRegistry()}
	monitor.containers.Store("container-id", &ContainerState{
		Image: "dbeaver/cloudbeaver:24.0.4",
		Ports: map[string]string{"8978": "18978"},
	})
	labels := map[string]string{
		"watchcow.enable":                  "true",
		"watchcow.redirect":                "https://example.com/",
		"watchcow.redirect_force_external": "true",
		"watchcow.ui_type":                 "iframe",
		"watchcow.all_users":               "false",
		"watchcow.file_types":              "sql,json",
		"watchcow.no_display":              "true",
		"watchcow.control.access_perm":     "readonly",
	}

	monitor.registerAppFromLabels("watchcow.cloudbeaver", "container-id", "cloudbeaver", labels)

	registered := monitor.registry.Get("watchcow.cloudbeaver")
	if registered == nil {
		t.Fatal("app was not registered")
	}
	entry := registered.GetEntry("")
	if entry == nil {
		t.Fatal("default entry was not restored from redirect label")
	}
	if entry.Redirect != labels["watchcow.redirect"] {
		t.Errorf("entry redirect = %q, want %q", entry.Redirect, labels["watchcow.redirect"])
	}
	if entry.Port != "18978" {
		t.Errorf("entry port = %q, want auto-detected host port %q", entry.Port, "18978")
	}
	if !entry.ForceExternal || entry.UIType != "iframe" || entry.AllUsers || !entry.NoDisplay {
		t.Errorf("entry fields were not fully restored: %+v", entry)
	}
	if !reflect.DeepEqual(entry.FileTypes, []string{"sql", "json"}) {
		t.Errorf("entry file types = %v, want [sql json]", entry.FileTypes)
	}
	if entry.Control == nil || entry.Control.AccessPerm != "readonly" {
		t.Errorf("entry control was not restored: %+v", entry.Control)
	}
	if registered.Image != "dbeaver/cloudbeaver:24.0.4" {
		t.Errorf("registered image = %q", registered.Image)
	}
}

func TestRegisterAppFromLabelsRestoresGeneratedDisplayName(t *testing.T) {
	t.Setenv("TRIM_DATA_SHARE_PATHS", "")
	t.Setenv("WATCHCOW_ICON_CDN_TEMPLATE", "https://icons.example/%s.png")
	monitor := &Monitor{registry: app.NewRegistry()}
	monitor.containers.Store("container-id", &ContainerState{
		Image: "example/my-app:latest",
		Ports: map[string]string{"8080": "18080"},
	})

	monitor.registerAppFromLabels("watchcow.my-app", "container-id", "my-app-1", map[string]string{
		"watchcow.enable": "true",
	})

	registered := monitor.registry.Get("watchcow.my-app")
	entry := registered.GetDefaultEntry()
	if registered.DisplayName != "My App" || registered.Port != "18080" ||
		registered.Icon != "https://icons.example/my-app.png" || entry == nil || entry.Icon != registered.Icon {
		t.Errorf("restored app differs from generated defaults: %+v", registered)
	}
}

func TestRegisterAppFromConfigPreservesCompleteApp(t *testing.T) {
	monitor := &Monitor{registry: app.NewRegistry()}
	config := &app.App{
		AppName:       "watchcow.test",
		Version:       "2.0.0",
		DisplayName:   "Test",
		Description:   "description",
		Maintainer:    "maintainer",
		ContainerID:   "old-id",
		ContainerName: "old-name",
		Image:         "test:latest",
		Protocol:      "https",
		Port:          "8443",
		Path:          "/app",
		UIType:        "iframe",
		AllUsers:      true,
		Entries: []app.Entry{{
			Name:          "admin",
			Title:         "Admin",
			Protocol:      "https",
			Port:          "8443",
			Path:          "/admin",
			UIType:        "iframe",
			AllUsers:      true,
			Icon:          "icon.png",
			FileTypes:     []string{"txt"},
			NoDisplay:     true,
			Control:       &app.EntryControl{AccessPerm: "readonly", PortPerm: "hidden", PathPerm: "editable"},
			Redirect:      "https://example.com",
			ForceExternal: true,
		}},
		Volumes:       []app.VolumeMapping{{Source: "/source", Destination: "/dest"}},
		Environment:   []string{"KEY=value"},
		Icon:          "app-icon.png",
		RestartPolicy: "unless-stopped",
		Labels:        map[string]string{"watchcow.enable": "true"},
		Status:        app.StatusPending,
	}
	want := *config
	want.ContainerID = "new-id"
	want.ContainerName = "new-name"
	want.Status = app.StatusRunning

	monitor.registerAppFromConfig(config, "new-id", "new-name")
	registered := monitor.registry.Get(config.AppName)
	if !reflect.DeepEqual(registered, &want) {
		t.Errorf("registered app did not preserve config\n got: %#v\nwant: %#v", registered, &want)
	}

	config.Entries[0].FileTypes[0] = "mutated"
	config.Entries[0].Control.AccessPerm = "mutated"
	config.Volumes[0].Source = "mutated"
	config.Environment[0] = "mutated"
	config.Labels["watchcow.enable"] = "mutated"
	if registered.Entries[0].FileTypes[0] != "txt" ||
		registered.Entries[0].Control.AccessPerm != "readonly" ||
		registered.Volumes[0].Source != "/source" ||
		registered.Environment[0] != "KEY=value" ||
		registered.Labels["watchcow.enable"] != "true" {
		t.Error("registered app shares mutable state with generation config")
	}
}

func TestRegisterAppFromStoredConfigPreservesFields(t *testing.T) {
	monitor := &Monitor{registry: app.NewRegistry()}
	monitor.containers.Store("container-id", &ContainerState{Image: "test:latest"})
	stored := &StoredConfig{
		AppName:     "watchcow.test",
		DisplayName: "Test",
		Description: "description",
		Version:     "1.0.0",
		Maintainer:  "maintainer",
		IconBase64:  "app-icon",
		Entries: []StoredEntry{{
			Title:         "",
			Protocol:      "https",
			Port:          "8443",
			Path:          "/app",
			UIType:        "iframe",
			AllUsers:      true,
			FileTypes:     []string{"txt"},
			NoDisplay:     true,
			Redirect:      "https://example.com",
			ForceExternal: true,
			IconBase64:    "entry-icon",
		}},
	}

	monitor.registerAppFromStoredConfig(stored, "container-id", "test")
	registered := monitor.registry.Get(stored.AppName)
	if registered == nil || len(registered.Entries) != 1 {
		t.Fatalf("stored app was not fully registered: %+v", registered)
	}
	entry := registered.Entries[0]
	if registered.Image != "test:latest" || registered.Icon != "app-icon" || entry.Title != "Test" ||
		registered.Port != "8443" || entry.Icon != "entry-icon" ||
		!entry.ForceExternal || !entry.NoDisplay || !reflect.DeepEqual(entry.FileTypes, []string{"txt"}) {
		t.Errorf("stored fields were not preserved: app=%+v entry=%+v", registered, entry)
	}
}

func TestAppConfigFromStoredPopulatesManifestPortFromUnnamedEntry(t *testing.T) {
	stored := &StoredConfig{
		AppName:     "watchcow.test",
		DisplayName: "Test",
		Entries: []StoredEntry{
			{Name: "admin", Title: "Admin", Port: "8443", Path: "/admin"},
			{Name: "", Title: "", Protocol: "http", Port: "8080", Path: "/", UIType: "url", AllUsers: true},
		},
	}

	config := appConfigFromStored(stored, "container-id", "test", "test:latest")
	if config.Port != "8080" || config.Protocol != "http" || config.Path != "/" || config.UIType != "url" || !config.AllUsers {
		t.Errorf("legacy manifest fields were not populated from unnamed entry: %+v", config)
	}
	if config.GetEntry("").Title != "Test" {
		t.Errorf("unnamed entry title = %q, want display name", config.GetEntry("").Title)
	}
	data := fpkgen.NewTemplateData(config)
	if data.Port != "8080" || data.DefaultLaunchEntry != "watchcow.test" {
		t.Errorf("template data does not match stored entry: %+v", data)
	}
}

func TestFirstHostPortUsesStableContainerPortOrder(t *testing.T) {
	ports := map[string]string{"10000": "110000", "3000": "13000"}
	if got := firstHostPort(ports); got != "13000" {
		t.Errorf("firstHostPort() = %q, want %q", got, "13000")
	}
}

func TestExtractPortsKeepsTCPMappingWhenUDPUsesSamePort(t *testing.T) {
	portMap := nat.PortMap{
		nat.Port("53/tcp"): {{HostPort: "1053"}},
		nat.Port("53/udp"): {{HostPort: "2053"}},
	}
	identityPorts, ports := extractPortMappings(portMap)
	if !reflect.DeepEqual(ports, map[string]string{"53": "1053"}) {
		t.Errorf("extractPorts() = %v, want TCP mapping only", ports)
	}
	if !reflect.DeepEqual(identityPorts, map[string]string{"53/tcp": "1053", "53/udp": "2053"}) {
		t.Errorf("identity ports lost protocol collision: %v", identityPorts)
	}
}

func TestPortExtractionUsesSameBindingForEventsAndScan(t *testing.T) {
	portMap := nat.PortMap{
		nat.Port("8080/tcp"): {
			{HostIP: "::", HostPort: "28080"},
			{HostIP: "0.0.0.0", HostPort: "18080"},
		},
	}
	_, eventPorts := extractPortMappings(portMap)
	_, scanPorts := extractSummaryPortMappings([]container.Port{
		{IP: "::", PrivatePort: 8080, PublicPort: 28080, Type: "tcp"},
		{IP: "0.0.0.0", PrivatePort: 8080, PublicPort: 18080, Type: "tcp"},
	})
	if !reflect.DeepEqual(eventPorts, map[string]string{"8080": "18080"}) || !reflect.DeepEqual(scanPorts, eventPorts) {
		t.Errorf("event ports %v and scan ports %v chose different bindings", eventPorts, scanPorts)
	}
	options := legacyPortOptions(portMap)
	if !reflect.DeepEqual(options, map[string][]string{"8080": {"18080", "28080"}}) {
		t.Errorf("legacy port options did not preserve alternate host binding: %v", options)
	}
	want := &StoredConfig{AppName: "watchcow.test"}
	monitor := &Monitor{configProvider: mapConfigProvider{"test:latest|8080:28080": want}}
	identityPorts, _ := extractPortMappings(portMap)
	if got := monitor.getStoredConfigForPorts("", "test:latest", identityPorts, options); got != want {
		t.Errorf("alternate binding storage lookup = %v, want %v", got, want)
	}
}

func TestContainerStateFromSummaryPreservesNetworkMode(t *testing.T) {
	ctr := container.Summary{
		ID:     "1234567890abcdef",
		Names:  []string{"/host-app"},
		Image:  "host-app:latest",
		State:  "running",
		Labels: map[string]string{"watchcow.enable": "true"},
	}
	ctr.HostConfig.NetworkMode = "host"

	state := containerStateFromSummary(ctr)
	if state.NetworkMode != "host" || state.ContainerID != "1234567890ab" || state.ContainerName != "host-app" {
		t.Fatalf("summary state lost runtime identity: %+v", state)
	}
}

func TestGetStoredConfigAcceptsLegacyProtocolCollisionKey(t *testing.T) {
	want := &StoredConfig{AppName: "watchcow.test"}
	monitor := &Monitor{configProvider: mapConfigProvider{
		"test:latest|53:2053,80:8080": want,
	}}
	ports := map[string]string{"53/tcp": "1053", "53/udp": "2053", "80/tcp": "8080"}
	if got := monitor.getStoredConfig("test:latest", ports); got != want {
		t.Errorf("getStoredConfig() = %v, want legacy config %v", got, want)
	}
}

func TestLegacyContainerKeyMatchingHasNoCombinationLimit(t *testing.T) {
	ports := make(map[string]string)
	options := make(map[string][]string)
	legacyPorts := make(map[string]string)
	for port := 1; port <= 12; port++ {
		containerPort := fmt.Sprintf("%d", port)
		ports[containerPort+"/tcp"] = fmt.Sprintf("1%03d", port)
		ports[containerPort+"/udp"] = fmt.Sprintf("2%03d", port)
		options[containerPort] = []string{fmt.Sprintf("1%03d", port), fmt.Sprintf("2%03d", port)}
		legacyPorts[containerPort] = fmt.Sprintf("2%03d", port)
	}
	legacyKey := makeContainerKey("test:latest", legacyPorts)
	want := &StoredConfig{AppName: "watchcow.test"}
	monitor := &Monitor{configProvider: mapConfigProvider{legacyKey: want}}
	if got := monitor.getStoredConfigForPorts("", "test:latest", ports, options); got != want {
		t.Errorf("lookup missed valid legacy key beyond old combination cap: got %v", got)
	}
}

func TestStoredConfigLookupRejectsAmbiguousLegacyOwnership(t *testing.T) {
	legacyKey := "dns:latest|53:1053"
	monitor := &Monitor{configProvider: mapConfigProvider{
		legacyKey: {AppName: "watchcow.dns"},
	}}
	monitor.containers.Store("tcp", &ContainerState{
		ContainerID:       "tcp",
		Image:             "dns:latest",
		Ports:             map[string]string{"53/tcp": "1053"},
		LegacyPortOptions: map[string][]string{"53": {"1053"}},
	})
	monitor.containers.Store("udp", &ContainerState{
		ContainerID:       "udp",
		Image:             "dns:latest",
		Ports:             map[string]string{"53/udp": "1053"},
		LegacyPortOptions: map[string][]string{"53": {"1053"}},
	})

	if got := monitor.getStoredConfigForPorts("tcp", "dns:latest", map[string]string{"53/tcp": "1053"}, map[string][]string{"53": {"1053"}}); got != nil {
		t.Fatalf("ambiguous legacy config was claimed: %+v", got)
	}
}

func TestStoredConfigLookupRejectsSharedNoPortLegacyKey(t *testing.T) {
	provider := mapConfigProvider{"redis:latest|": {AppName: "watchcow.redis"}}
	monitor := &Monitor{configProvider: provider}
	monitor.containers.Store("a", &ContainerState{ContainerID: "a", ContainerName: "redis-a", Image: "redis:latest"})
	monitor.containers.Store("b", &ContainerState{ContainerID: "b", ContainerName: "redis-b", Image: "redis:latest"})

	if got := monitor.getStoredConfigForPorts("a", "redis:latest", nil, nil); got != nil {
		t.Fatalf("shared no-port legacy config was claimed: %+v", got)
	}
	if makeContainerKeyForName("redis:latest", "redis-a", nil) == makeContainerKeyForName("redis:latest", "redis-b", nil) {
		t.Fatal("no-port canonical keys collided")
	}
}

func TestGetContainerByKeyPrefersExactNamedOwner(t *testing.T) {
	monitor := &Monitor{}
	for _, name := range []string{"web-a", "web-b"} {
		monitor.containers.Store(name, &ContainerState{
			ContainerID: name, ContainerName: name, Image: "web:latest",
			Ports: map[string]string{"8080/tcp": "18080"}, LegacyPortOptions: map[string][]string{"8080": {"18080"}},
		})
	}
	if id, ok := monitor.GetContainerByKey("web:latest|@web-a;8080/tcp:18080"); !ok || id != "web-a" {
		t.Fatalf("exact named key resolved to %q, %v", id, ok)
	}
	if id, ok := monitor.GetContainerByKey("web:latest|8080:18080"); ok {
		t.Fatalf("ambiguous legacy key resolved to %q", id)
	}
}

func TestStoredConfigLookupMigratesUniqueLegacyKey(t *testing.T) {
	legacyKey := "web:latest|8080:18080"
	canonicalKey := "web:latest|@web;8080/tcp:18080"
	provider := mapConfigProvider{legacyKey: {AppName: "watchcow.web"}}
	monitor := &Monitor{configProvider: provider}
	monitor.containers.Store("web", &ContainerState{
		ContainerID:       "web",
		ContainerName:     "web",
		Image:             "web:latest",
		Ports:             map[string]string{"8080/tcp": "18080"},
		LegacyPortOptions: map[string][]string{"8080": {"18080"}},
	})

	config := monitor.getStoredConfigForPorts("web", "web:latest", map[string]string{"8080/tcp": "18080"}, map[string][]string{"8080": {"18080"}})
	if config == nil || config.Key != canonicalKey || provider[legacyKey] != nil || provider[canonicalKey] == nil {
		t.Fatalf("unique legacy config was not migrated: config=%+v provider=%+v", config, provider)
	}
}

func TestStoredConfigLookupMigratesRenamedContainerKey(t *testing.T) {
	oldKey := "web:latest|@old-name;8080/tcp:18080"
	newKey := "web:latest|@new-name;8080/tcp:18080"
	provider := mapConfigProvider{oldKey: {AppName: "watchcow.web"}}
	monitor := &Monitor{configProvider: provider}
	monitor.containers.Store("web", &ContainerState{
		ContainerID: "web", ContainerName: "new-name", Image: "web:latest",
		Ports: map[string]string{"8080/tcp": "18080"}, LegacyPortOptions: map[string][]string{"8080": {"18080"}},
	})

	config := monitor.getStoredConfigForPorts("web", "web:latest", map[string]string{"8080/tcp": "18080"}, map[string][]string{"8080": {"18080"}})
	if config == nil || config.Key != newKey || provider[oldKey] != nil || provider[newKey] == nil {
		t.Fatalf("renamed config was not migrated: config=%+v provider=%+v", config, provider)
	}
}

func TestStoredConfigLookupFindsSameNameAfterImageAndPortChange(t *testing.T) {
	oldKey := "web:1.0|@web;8080/tcp:18080"
	newKey := "web:2.0|@web;8080/tcp:28080"
	provider := mapConfigProvider{oldKey: {AppName: "watchcow.web"}}
	monitor := &Monitor{configProvider: provider}
	monitor.containers.Store("web", &ContainerState{
		ContainerID: "web", ContainerName: "web", Image: "web:2.0",
		Ports: map[string]string{"8080/tcp": "28080"}, LegacyPortOptions: map[string][]string{"8080": {"28080"}},
	})

	config := monitor.getStoredConfigForPorts("web", "web:2.0", map[string]string{"8080/tcp": "28080"}, map[string][]string{"8080": {"28080"}})
	if config == nil || config.Key != newKey || provider[oldKey] != nil || provider[newKey] == nil {
		t.Fatalf("same-name config was not recovered: config=%+v provider=%+v", config, provider)
	}
}

func TestDestroyReconcilesNewlyUniqueLegacyConfig(t *testing.T) {
	provider := mapConfigProvider{"redis:latest|": {AppName: "watchcow.redis"}}
	monitor := &Monitor{
		configProvider: provider,
		registry:       app.NewRegistry(),
		opQueue:        make(chan *AppOperation, 2),
	}
	monitor.containers.Store("a", &ContainerState{
		ContainerID: "a", ContainerName: "redis-a", Image: "redis:latest", State: "running",
	})
	monitor.containers.Store("b", &ContainerState{
		ContainerID: "b", ContainerName: "redis-b", Image: "redis:latest", State: "running",
	})

	monitor.processDestroy(&AppOperation{ContainerID: "b"})
	select {
	case op := <-monitor.opQueue:
		if op.ContainerID != "a" || op.StoredConfig == nil || op.StoredConfig.Key != "redis:latest|@redis-a" {
			t.Fatalf("unexpected reconciliation operation: %+v", op)
		}
	default:
		t.Fatal("destroy did not reconcile the newly unique legacy config")
	}
}

func TestStoredConfigLookupRejectsMultipleLegacyCandidates(t *testing.T) {
	monitor := &Monitor{configProvider: mapConfigProvider{
		"web:latest|8080:9000":  {AppName: "watchcow.old-a"},
		"web:latest|8080:10000": {AppName: "watchcow.old-b"},
	}}
	options := map[string][]string{"8080": {"9000", "10000"}}
	if got := monitor.getStoredConfigForPorts("", "web:latest", map[string]string{"8080/tcp": "9000"}, options); got != nil {
		t.Fatalf("multiple legacy configs were resolved arbitrarily: %+v", got)
	}
}

func TestDashboardReinstallDropsStaleRevisionBeforeUninstall(t *testing.T) {
	key := "web:latest|8080/tcp:18080"
	current := &StoredConfig{Key: key, AppName: "watchcow.web", Revision: "b", Pending: true}
	installer := &fakeAppInstaller{}
	monitor := &Monitor{
		installer:      installer,
		registry:       app.NewRegistry(),
		configProvider: mapConfigProvider{key: current},
	}
	monitor.containers.Store("container-id", &ContainerState{
		ContainerID: "container-id",
		AppName:     "watchcow.web",
		Installed:   true,
	})

	monitor.processDashboardReinstall(context.Background(), &AppOperation{
		ContainerID: "container-id",
		AppName:     "watchcow.web",
		StoredConfig: &StoredConfig{
			Key:      key,
			AppName:  "watchcow.web",
			Revision: "a",
			Pending:  true,
		},
	})
	if len(installer.uninstallCalls) != 0 {
		t.Fatalf("stale revision performed uninstall: %v", installer.uninstallCalls)
	}
}

func TestDashboardUninstallDropsStaleDeleteAfterNewSave(t *testing.T) {
	key := "web:latest|8080/tcp:18080"
	installer := &fakeAppInstaller{}
	monitor := &Monitor{
		installer:      installer,
		registry:       app.NewRegistry(),
		configProvider: mapConfigProvider{key: {Key: key, Revision: "new", Pending: true}},
	}
	monitor.processDashboardUninstall(&AppOperation{
		AppName:   "watchcow.web",
		ConfigKey: key,
	})
	if len(installer.uninstallCalls) != 0 {
		t.Fatalf("stale delete uninstalled newly saved app: %v", installer.uninstallCalls)
	}
}

func TestDashboardUninstallCompletesDurableDeleteOnlyAfterSuccess(t *testing.T) {
	key := "web:latest|@web;8080/tcp:18080"
	for _, test := range []struct {
		name       string
		installed  bool
		uninstall  error
		wantConfig bool
		wantCalls  int
	}{
		{name: "installed success", installed: true, wantCalls: 1},
		{name: "not installed", wantCalls: 0},
		{name: "failure", installed: true, uninstall: errors.New("uninstall failed"), wantConfig: true, wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := mapConfigProvider{key: {Key: key, AppName: "watchcow.web", Deleting: true}}
			installer := &fakeAppInstaller{installed: test.installed, uninstallErr: test.uninstall}
			registry := app.NewRegistry()
			registry.Register(&app.App{AppName: "watchcow.web", Status: app.StatusRunning})
			monitor := &Monitor{installer: installer, registry: registry, configProvider: provider}
			monitor.containers.Store("web", &ContainerState{ContainerID: "web", AppName: "watchcow.web", Installed: true})

			monitor.processDashboardUninstall(&AppOperation{ContainerID: "web", AppName: "watchcow.web", ConfigKey: key})
			config := provider[key]
			if (config != nil) != test.wantConfig {
				t.Fatalf("config after uninstall = %+v, want present=%v", config, test.wantConfig)
			}
			if len(installer.uninstallCalls) != test.wantCalls {
				t.Fatalf("uninstall calls = %v, want %d", installer.uninstallCalls, test.wantCalls)
			}
			if test.uninstall != nil {
				if config == nil || config.LastError != test.uninstall.Error() || registry.Get("watchcow.web") == nil {
					t.Fatalf("failed uninstall was not recoverable: config=%+v app=%+v", config, registry.Get("watchcow.web"))
				}
			} else if registry.Get("watchcow.web") != nil {
				t.Fatal("successful uninstall left registry ownership")
			}
		})
	}
}

func TestProtocolQualifiedIdentityDistinguishesSinglePortProtocol(t *testing.T) {
	tcp, _ := extractPortMappings(nat.PortMap{nat.Port("53/tcp"): {{HostPort: "1053"}}})
	udp, _ := extractPortMappings(nat.PortMap{nat.Port("53/udp"): {{HostPort: "1053"}}})
	if makeContainerKey("dns:latest", tcp) == makeContainerKey("dns:latest", udp) {
		t.Fatalf("TCP and UDP identity keys collided: tcp=%v udp=%v", tcp, udp)
	}
}

func TestListAllContainersReturnsIndependentPortSnapshots(t *testing.T) {
	monitor := &Monitor{}
	monitor.containers.Store("container-id", &ContainerState{
		ContainerID:       "container-id",
		Image:             "test:latest",
		Ports:             map[string]string{"8080/tcp": "18080"},
		WebPorts:          map[string]string{"8080": "18080"},
		LegacyPortOptions: map[string][]string{"8080": {"18080", "28080"}},
	})

	containers, err := monitor.ListAllContainers(context.Background())
	if err != nil || len(containers) != 1 {
		t.Fatalf("ListAllContainers() = %v, %v", containers, err)
	}
	containers[0].IdentityPorts["8080/tcp"] = "mutated"
	containers[0].LegacyPortOptions["8080"][0] = "mutated"

	value, _ := monitor.containers.Load("container-id")
	state := value.(*ContainerState)
	if state.Ports["8080/tcp"] != "18080" || state.LegacyPortOptions["8080"][0] != "18080" {
		t.Errorf("ListAllContainers returned shared port state: %+v", state)
	}
}

func TestInstalledAppNameRechecksDashboardOperationAtExecution(t *testing.T) {
	monitor := &Monitor{}
	monitor.containers.Store("container-id", &ContainerState{AppName: "watchcow.test", Installed: true})
	if got := monitor.installedAppName("container-id"); got != "watchcow.test" {
		t.Errorf("installedAppName() = %q, want queued install upgraded to reinstall", got)
	}
}

func TestDashboardReinstallPreservesStateWhenUninstallFails(t *testing.T) {
	installer := &fakeAppInstaller{uninstallErr: errors.New("uninstall failed")}
	registry := app.NewRegistry()
	registry.Register(&app.App{AppName: "watchcow.test"})
	monitor := &Monitor{installer: installer, registry: registry}
	monitor.containers.Store("container-id", &ContainerState{
		ContainerID: "container-id",
		AppName:     "watchcow.test",
		Installed:   true,
	})

	monitor.processDashboardReinstall(context.Background(), &AppOperation{
		ContainerID: "container-id",
		AppName:     "watchcow.test",
		StoredConfig: &StoredConfig{
			AppName: "watchcow.test",
		},
	})

	value, _ := monitor.containers.Load("container-id")
	state := value.(*ContainerState)
	if state.AppName != "watchcow.test" || !state.Installed {
		t.Errorf("failed uninstall cleared installed state: %+v", state)
	}
	if registry.Get("watchcow.test") == nil {
		t.Error("failed uninstall removed the live registry entry")
	}
	if !reflect.DeepEqual(installer.uninstallCalls, []string{"watchcow.test"}) {
		t.Errorf("uninstall calls = %v", installer.uninstallCalls)
	}
}

func TestDashboardReinstallMarksRegistryStoppedWhenRestoreFails(t *testing.T) {
	installer := &fakeAppInstaller{uninstallErr: &fpkgen.RestoreStoppedAppError{
		UninstallErr: errors.New("uninstall failed"),
		RestoreErr:   errors.New("restart failed"),
	}}
	registry := app.NewRegistry()
	registry.Register(&app.App{AppName: "watchcow.test", Status: app.StatusRunning})
	monitor := &Monitor{installer: installer, registry: registry}
	monitor.containers.Store("container-id", &ContainerState{
		ContainerID: "container-id",
		AppName:     "watchcow.test",
		Installed:   true,
	})

	monitor.processDashboardReinstall(context.Background(), &AppOperation{
		ContainerID: "container-id",
		StoredConfig: &StoredConfig{
			AppName: "watchcow.test",
		},
	})
	if got := registry.Get("watchcow.test"); got == nil || got.Status != app.StatusStopped {
		t.Fatalf("registry status after failed restore = %+v, want stopped", got)
	}
}

func TestPendingStoredConfigForcesReconcileOnStartup(t *testing.T) {
	installer := &fakeAppInstaller{installed: true, uninstallErr: errors.New("stop here")}
	monitor := &Monitor{installer: installer, registry: app.NewRegistry()}
	monitor.containers.Store("container-id", &ContainerState{
		ContainerID: "container-id",
		AppName:     "watchcow.test",
		Installed:   true,
	})

	monitor.processContainerStart(context.Background(), &AppOperation{
		ContainerID: "container-id",
		StoredConfig: &StoredConfig{
			AppName:  "watchcow.test",
			Pending:  true,
			Revision: "revision-b",
		},
	})

	if !reflect.DeepEqual(installer.uninstallCalls, []string{"watchcow.test"}) {
		t.Fatalf("pending config did not enter reinstall path: calls=%v", installer.uninstallCalls)
	}
}

func TestPendingConfigUninstallsPackageMissingFromState(t *testing.T) {
	key := "web:latest|8080/tcp:18080"
	config := &StoredConfig{Key: key, AppName: "watchcow.web", Revision: "b", Pending: true}
	installer := &fakeAppInstaller{installed: true, uninstallErr: errors.New("stop after uninstall attempt")}
	monitor := &Monitor{
		installer:      installer,
		registry:       app.NewRegistry(),
		configProvider: mapConfigProvider{key: config},
	}
	monitor.containers.Store("container-id", &ContainerState{ContainerID: "container-id"})

	monitor.processDashboardReinstall(context.Background(), &AppOperation{
		ContainerID:  "container-id",
		StoredConfig: config,
	})
	if !reflect.DeepEqual(installer.uninstallCalls, []string{"watchcow.web"}) {
		t.Fatalf("installed package missing from state was not uninstalled: %v", installer.uninstallCalls)
	}
}

func TestChangedLabelRevisionForcesReinstallAcrossMonitorRestart(t *testing.T) {
	oldLabels := map[string]string{
		"watchcow.enable":       "true",
		"watchcow.service_port": "8080",
	}
	newLabels := map[string]string{
		"watchcow.enable":       "true",
		"watchcow.service_port": "9090",
	}
	state := &ContainerState{
		ContainerID:   "container-id",
		ContainerName: "web",
		Image:         "web:latest",
		State:         "running",
		WebPorts:      map[string]string{"8080": "18080"},
		Labels:        newLabels,
		NetworkMode:   "bridge",
	}
	oldRevision := labelConfigRevision(state.ContainerName, state.Image, state.NetworkMode, state.WebPorts, oldLabels)
	newRevision := labelRevisionForState(state)
	installer := &fakeAppInstaller{installed: true, uninstallErr: errors.New("stop after uninstall attempt")}
	monitor := &Monitor{
		installer:      installer,
		registry:       app.NewRegistry(),
		labelRevisions: map[string]string{"watchcow.web": oldRevision},
	}
	monitor.containers.Store(state.ContainerID, state)

	monitor.processContainerStart(context.Background(), &AppOperation{
		ContainerID:   state.ContainerID,
		ContainerName: state.ContainerName,
		Labels:        newLabels,
		LabelRevision: newRevision,
	})

	if !reflect.DeepEqual(installer.uninstallCalls, []string{"watchcow.web"}) {
		t.Fatalf("changed label revision reused installed package: uninstall calls=%v", installer.uninstallCalls)
	}
}

func TestMatchingLabelRevisionReusesInstalledPackage(t *testing.T) {
	labels := map[string]string{"watchcow.enable": "true", "watchcow.service_port": "8080"}
	state := &ContainerState{
		ContainerID:   "container-id",
		ContainerName: "web",
		Image:         "web:latest",
		State:         "running",
		WebPorts:      map[string]string{"8080": "18080"},
		Labels:        labels,
		NetworkMode:   "bridge",
	}
	revision := labelRevisionForState(state)
	installer := &fakeAppInstaller{installed: true}
	monitor := &Monitor{
		installer:      installer,
		registry:       app.NewRegistry(),
		labelRevisions: map[string]string{"watchcow.web": revision},
	}
	monitor.containers.Store(state.ContainerID, state)

	monitor.processContainerStart(context.Background(), &AppOperation{
		ContainerID:   state.ContainerID,
		ContainerName: state.ContainerName,
		Labels:        labels,
		LabelRevision: revision,
	})

	if len(installer.uninstallCalls) != 0 || !reflect.DeepEqual(installer.startCalls, []string{"watchcow.web"}) {
		t.Fatalf("matching label revision did not reuse package: uninstall=%v start=%v", installer.uninstallCalls, installer.startCalls)
	}
	if got := monitor.registry.Get("watchcow.web"); got == nil || got.Status != app.StatusRunning {
		t.Fatalf("reused label app registry = %+v, want running", got)
	}
}

func TestLabelRevisionPersistsAcrossMonitorInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), labelRevisionsFilename)
	first := &Monitor{labelRevisions: make(map[string]string), labelRevisionsPath: path}
	if err := first.markLabelRevisionAppliedForContainer("watchcow.web", "revision-a", "web"); err != nil {
		t.Fatal(err)
	}

	state, err := loadPersistedMonitorState(path)
	if err != nil {
		t.Fatal(err)
	}
	second := &Monitor{
		labelRevisions: state.LabelRevisions, labelOwners: state.LabelOwners,
		pendingUninstalls: state.PendingUninstalls, labelRevisionsPath: path,
	}
	if !second.isLabelRevisionApplied("watchcow.web", "revision-a") {
		t.Fatalf("applied label revision did not survive restart: %v", state.LabelRevisions)
	}
	if got := second.appliedLabelAppForContainer("web"); got != "watchcow.web" {
		t.Fatalf("applied label owner did not survive restart: %q", got)
	}
	if err := second.clearLabelRevision("watchcow.web"); err != nil {
		t.Fatal(err)
	}
	state, err = loadPersistedMonitorState(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.LabelRevisions["watchcow.web"]; ok || len(state.LabelOwners) != 0 {
		t.Fatalf("cleared label state reappeared after restart: %+v", state)
	}
}

func TestSourceSwitchUninstallsPreviouslyAppliedPackage(t *testing.T) {
	for _, test := range []struct {
		name       string
		op         *AppOperation
		provider   mapConfigProvider
		previous   string
		desired    string
		labelState bool
	}{
		{
			name: "dashboard to labels with different app name",
			op: &AppOperation{ContainerID: "web", ContainerName: "web", Labels: map[string]string{
				"watchcow.enable": "true", "watchcow.appname": "watchcow.label-web",
			}, PreviousAppName: "watchcow.dashboard-web"},
			previous: "watchcow.dashboard-web", desired: "watchcow.label-web",
		},
		{
			name: "labels to dashboard with different app name",
			op: &AppOperation{ContainerID: "web", ContainerName: "web", StoredConfig: &StoredConfig{
				Key: "web|@web", AppName: "watchcow.dashboard-web", Revision: "a",
			}, PreviousAppName: "watchcow.label-web"},
			provider: mapConfigProvider{"web|@web": {Key: "web|@web", AppName: "watchcow.dashboard-web", Revision: "a"}},
			previous: "watchcow.label-web", desired: "watchcow.dashboard-web",
		},
		{
			name: "labels to dashboard with the same app name",
			op: &AppOperation{ContainerID: "web", ContainerName: "web", StoredConfig: &StoredConfig{
				Key: "web|@web", AppName: "watchcow.web", Revision: "a",
			}},
			provider: mapConfigProvider{"web|@web": {Key: "web|@web", AppName: "watchcow.web", Revision: "a"}},
			previous: "watchcow.web", desired: "watchcow.web", labelState: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			uninstallErr := errors.New("stop after source-switch uninstall")
			installer := &fakeAppInstaller{
				installedApps: map[string]bool{test.previous: true, test.desired: test.previous == test.desired},
				uninstallErr:  uninstallErr,
			}
			monitor := &Monitor{
				installer: installer, registry: app.NewRegistry(), configProvider: test.provider,
				labelRevisions: make(map[string]string), labelOwners: make(map[string]string),
				pendingUninstalls: make(map[string]bool),
			}
			if test.labelState {
				monitor.labelRevisions[test.previous] = "label-revision"
				monitor.labelOwners["web"] = test.previous
			}
			monitor.containers.Store("web", &ContainerState{ContainerID: "web", ContainerName: "web", State: "running"})

			monitor.processContainerStart(context.Background(), test.op)
			if !reflect.DeepEqual(installer.uninstallCalls, []string{test.previous}) {
				t.Fatalf("source switch did not uninstall %q: calls=%v", test.previous, installer.uninstallCalls)
			}
		})
	}
}

func TestRemovalIntentsRecoverWithoutContainer(t *testing.T) {
	provider := mapConfigProvider{
		"web|@web":     {Key: "web|@web", AppName: "watchcow.dashboard-web", Deleting: true},
		"notes|@notes": {Key: "notes|@notes", AppName: "watchcow.dashboard-notes"},
	}
	monitor := &Monitor{
		configProvider: provider, opQueue: make(chan *AppOperation, 4),
		installer:      &fakeAppInstaller{installedApps: map[string]bool{"watchcow.dashboard-notes": true}},
		labelRevisions: map[string]string{"watchcow.label-web": "revision"},
		labelOwners:    map[string]string{"web": "watchcow.label-web"}, pendingUninstalls: make(map[string]bool),
	}
	monitor.inventoryReady.Store(true)

	monitor.reconcileRemovalIntents()
	seen := make(map[string]string)
	for len(monitor.opQueue) > 0 {
		op := <-monitor.opQueue
		seen[op.AppName] = op.Type
	}
	if seen["watchcow.dashboard-web"] != "dashboard_uninstall" || seen["watchcow.dashboard-notes"] != "orphan_uninstall" || seen["watchcow.label-web"] != "orphan_uninstall" {
		t.Fatalf("removal intents were not recovered: %v", seen)
	}
	if !monitor.isPendingUninstall("watchcow.label-web") {
		t.Fatal("orphaned label app was not persisted for retry")
	}
}

func TestRemovalIntentsWaitForAuthoritativeInventory(t *testing.T) {
	monitor := &Monitor{
		configProvider: mapConfigProvider{
			"web|@web": {Key: "web|@web", AppName: "watchcow.web", Deleting: true},
		},
		installer:         &fakeAppInstaller{installedApps: map[string]bool{"watchcow.web": true}},
		opQueue:           make(chan *AppOperation, 1),
		labelRevisions:    make(map[string]string),
		labelOwners:       map[string]string{"web": "watchcow.web"},
		pendingUninstalls: make(map[string]bool),
	}

	monitor.reconcileRemovalIntents()
	if len(monitor.opQueue) != 0 || monitor.isPendingUninstall("watchcow.web") {
		t.Fatal("failed Docker inventory was treated as authoritative removal")
	}
}

func TestLabelAppConflictIsDetectedForRunningOwners(t *testing.T) {
	monitor := &Monitor{}
	labels := map[string]string{"watchcow.enable": "true", "watchcow.appname": "shared.app"}
	monitor.containers.Store("a", &ContainerState{ContainerID: "a", ContainerName: "a", State: "running", Labels: labels})
	monitor.containers.Store("b", &ContainerState{ContainerID: "b", ContainerName: "b", State: "running", Labels: labels})
	if !monitor.labelAppHasMultipleOwners("a", "shared.app") {
		t.Fatal("duplicate running label owners were not detected")
	}
	monitor.updateContainerState("b", nil, func(state *ContainerState) { state.State = "exited" })
	if monitor.labelAppHasMultipleOwners("a", "shared.app") {
		t.Fatal("stopped replacement container should not block the running owner")
	}
}

func TestBlockedDashboardConfigIsNotQueued(t *testing.T) {
	monitor := &Monitor{opQueue: make(chan *AppOperation, 1)}
	monitor.queueStoredConfigOperation("web", "web", nil, &StoredConfig{
		AppName: "watchcow.web", LastError: "需要配置端口",
	})
	if len(monitor.opQueue) != 0 {
		t.Fatal("dashboard config requiring user input was queued for installation")
	}
	monitor.processContainerStart(context.Background(), &AppOperation{StoredConfig: &StoredConfig{
		AppName: "watchcow.web", LastError: "需要配置端口",
	}})
}

func TestEmptyLegacyRevisionDoesNotMatchNewStoredConfig(t *testing.T) {
	provider := mapConfigProvider{
		"web|@web": {Key: "web|@web", AppName: "watchcow.web", Revision: "new", Pending: true},
	}
	monitor := &Monitor{configProvider: provider}
	legacy := &StoredConfig{Key: "web|@web", AppName: "watchcow.web"}
	if monitor.isCurrentStoredConfig(legacy) || monitor.isCurrentPendingConfig(legacy) {
		t.Fatal("empty legacy revision matched a newer stored config")
	}
	monitor.recordDashboardFailure(legacy, errors.New("old failure"))
	if provider["web|@web"].LastError != "" {
		t.Fatal("stale legacy failure was written to the new config")
	}
}

func TestDestroyFailurePersistsCleanupAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), labelRevisionsFilename)
	installer := &fakeAppInstaller{installedApps: map[string]bool{"watchcow.web": true}, uninstallErr: errors.New("uninstall failed")}
	first := &Monitor{
		installer: installer, registry: app.NewRegistry(), labelRevisionsPath: path,
		labelRevisions: make(map[string]string), labelOwners: make(map[string]string), pendingUninstalls: make(map[string]bool),
	}
	first.containers.Store("web", &ContainerState{ContainerID: "web", AppName: "watchcow.web", Installed: true})
	first.processDestroy(&AppOperation{ContainerID: "web"})

	state, err := loadPersistedMonitorState(path)
	if err != nil {
		t.Fatal(err)
	}
	second := &Monitor{
		opQueue: make(chan *AppOperation, 1), labelRevisions: state.LabelRevisions,
		labelOwners: state.LabelOwners, pendingUninstalls: state.PendingUninstalls,
	}
	second.inventoryReady.Store(true)
	second.reconcileRemovalIntents()
	select {
	case op := <-second.opQueue:
		if op.Type != "orphan_uninstall" || op.AppName != "watchcow.web" {
			t.Fatalf("unexpected recovered cleanup: %+v", op)
		}
	default:
		t.Fatal("destroy failure was not recovered after restart")
	}
}

func TestLabelRevisionWriteFailureRollsBackMemory(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	monitor := &Monitor{
		labelRevisions:     map[string]string{"watchcow.web": "old"},
		labelRevisionsPath: filepath.Join(blocker, labelRevisionsFilename),
	}

	if err := monitor.markLabelRevisionApplied("watchcow.web", "new"); err == nil {
		t.Fatal("markLabelRevisionApplied succeeded with an invalid storage path")
	}
	if got := monitor.labelRevisions["watchcow.web"]; got != "old" {
		t.Fatalf("failed mark changed memory to %q", got)
	}
	if err := monitor.clearLabelRevision("watchcow.web"); err == nil {
		t.Fatal("clearLabelRevision succeeded with an invalid storage path")
	}
	if got := monitor.labelRevisions["watchcow.web"]; got != "old" {
		t.Fatalf("failed clear changed memory to %q", got)
	}
}

func TestDashboardReinstallClearsEveryPreviousOwner(t *testing.T) {
	key := "web:latest|@web;8080/tcp:18080"
	current := &StoredConfig{Key: key, AppName: "watchcow.web", Revision: "a", Pending: true}
	provider := mapConfigProvider{key: current}
	installer := &fakeAppInstaller{}
	installer.onUninstall = func() {
		provider[key] = &StoredConfig{Key: key, AppName: "watchcow.web", Revision: "b", Pending: true}
	}
	monitor := &Monitor{
		installer:      installer,
		registry:       app.NewRegistry(),
		configProvider: provider,
	}
	for _, id := range []string{"old-container", "new-container"} {
		monitor.containers.Store(id, &ContainerState{ContainerID: id, AppName: "watchcow.web", Installed: true})
	}

	monitor.processDashboardReinstall(context.Background(), &AppOperation{
		ContainerID:  "new-container",
		StoredConfig: current,
	})

	for _, id := range []string{"old-container", "new-container"} {
		value, _ := monitor.containers.Load(id)
		state := value.(*ContainerState)
		if state.Installed || state.AppName != "" {
			t.Errorf("owner %s survived successful uninstall: %+v", id, state)
		}
	}
	monitor.processDestroy(&AppOperation{ContainerID: "old-container"})
	if len(installer.uninstallCalls) != 1 {
		t.Fatalf("destroy of cleared owner uninstalled replacement package: %v", installer.uninstallCalls)
	}
}

func TestDashboardUninstallDoesNotRemoveLabelOwnedApp(t *testing.T) {
	installer := &fakeAppInstaller{}
	monitor := &Monitor{installer: installer, registry: app.NewRegistry()}
	monitor.containers.Store("container-id", &ContainerState{
		ContainerID:   "container-id",
		ContainerName: "web",
		Labels: map[string]string{
			"watchcow.enable":  "true",
			"watchcow.appname": "watchcow.shared",
		},
		AppName:   "watchcow.shared",
		Installed: true,
	})

	monitor.processDashboardUninstall(&AppOperation{ContainerID: "container-id", AppName: "watchcow.shared"})
	if len(installer.uninstallCalls) != 0 {
		t.Fatalf("dashboard uninstall removed label-owned app: %v", installer.uninstallCalls)
	}
}

func TestProcessStopUpdatesRegistryOnlyAfterSuccessfulStop(t *testing.T) {
	for _, test := range []struct {
		name       string
		stopErr    error
		wantStatus app.Status
	}{
		{name: "success", wantStatus: app.StatusStopped},
		{name: "failure", stopErr: errors.New("stop failed"), wantStatus: app.StatusRunning},
	} {
		t.Run(test.name, func(t *testing.T) {
			installer := &fakeAppInstaller{stopErr: test.stopErr}
			registry := app.NewRegistry()
			registry.Register(&app.App{AppName: "watchcow.web", Status: app.StatusRunning})
			monitor := &Monitor{installer: installer, registry: registry}
			monitor.containers.Store("container-id", &ContainerState{
				ContainerID: "container-id",
				AppName:     "watchcow.web",
				Installed:   true,
			})

			monitor.processStop(&AppOperation{ContainerID: "container-id"})
			if got := registry.Get("watchcow.web").Status; got != test.wantStatus {
				t.Fatalf("registry status = %q, want %q", got, test.wantStatus)
			}
		})
	}
}

func TestExistingAppStartResultControlsRegistryStatus(t *testing.T) {
	for _, test := range []struct {
		name       string
		startErr   error
		wantStatus app.Status
	}{
		{name: "success", wantStatus: app.StatusRunning},
		{name: "failure", startErr: errors.New("start failed"), wantStatus: app.StatusStopped},
	} {
		t.Run(test.name, func(t *testing.T) {
			installer := &fakeAppInstaller{installed: true, startErr: test.startErr}
			registry := app.NewRegistry()
			monitor := &Monitor{installer: installer, registry: registry}
			monitor.containers.Store("container-id", &ContainerState{ContainerID: "container-id"})

			monitor.processContainerStart(context.Background(), &AppOperation{
				ContainerID:   "container-id",
				ContainerName: "web",
				StoredConfig:  &StoredConfig{AppName: "watchcow.web"},
			})
			if got := registry.Get("watchcow.web").Status; got != test.wantStatus {
				t.Fatalf("registry status = %q, want %q", got, test.wantStatus)
			}
		})
	}
}

func TestUpdateContainerStatePublishesCompleteSnapshots(t *testing.T) {
	monitor := &Monitor{}
	monitor.containers.Store("container-id", &ContainerState{
		ContainerID: "container-id",
		Ports:       map[string]string{"80": "8080"},
		Labels:      map[string]string{"watchcow.enable": "true"},
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 1000 {
			monitor.updateContainerState("container-id", nil, func(state *ContainerState) {
				state.AppName = "watchcow.test"
				state.Installed = true
			})
		}
	}()
	go func() {
		defer wg.Done()
		for range 1000 {
			monitor.updateContainerState("container-id", nil, func(state *ContainerState) {
				state.State = "running"
				state.Image = "test:latest"
			})
		}
	}()
	wg.Wait()

	value, _ := monitor.containers.Load("container-id")
	state := value.(*ContainerState)
	if state.AppName != "watchcow.test" || !state.Installed || state.State != "running" || state.Image != "test:latest" {
		t.Errorf("concurrent updates lost state: %+v", state)
	}
}
