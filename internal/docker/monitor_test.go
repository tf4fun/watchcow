package docker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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
	uninstallCalls  []string
	installedChecks []string
	installed       bool
}

func (i *fakeAppInstaller) InstallLocal(string, map[string]string) error { return nil }
func (i *fakeAppInstaller) StartApp(string) error                        { return nil }
func (i *fakeAppInstaller) StopApp(string) error                         { return nil }
func (i *fakeAppInstaller) IsAppInstalled(appName string) bool {
	i.installedChecks = append(i.installedChecks, appName)
	return i.installed
}
func (i *fakeAppInstaller) Uninstall(appName string) error {
	i.uninstallCalls = append(i.uninstallCalls, appName)
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

func (p mapConfigProvider) MarkApplied(key, revision string) error {
	if config := p[key]; config != nil && config.Revision == revision {
		config.Pending = false
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
	monitor := &Monitor{registry: app.NewRegistry()}
	monitor.containers.Store("container-id", &ContainerState{
		Image: "example/my-app:latest",
		Ports: map[string]string{"8080": "18080"},
	})

	monitor.registerAppFromLabels("watchcow.my-app", "container-id", "my-app-1", map[string]string{
		"watchcow.enable": "true",
	})

	registered := monitor.registry.Get("watchcow.my-app")
	if registered.DisplayName != "My App" || registered.Port != "18080" {
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
			Title:         "Default",
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
	if registered.Image != "test:latest" || registered.Icon != "app-icon" ||
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
