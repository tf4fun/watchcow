package server

import (
	"testing"
)

func TestNewContainerKey(t *testing.T) {
	tests := []struct {
		name     string
		image    string
		ports    map[string]string
		expected ContainerKey
	}{
		{
			name:     "no ports",
			image:    "nginx:alpine",
			ports:    nil,
			expected: "nginx:alpine|",
		},
		{
			name:     "empty ports map",
			image:    "nginx:alpine",
			ports:    map[string]string{},
			expected: "nginx:alpine|",
		},
		{
			name:  "single port",
			image: "nginx:alpine",
			ports: map[string]string{
				"80": "8080",
			},
			expected: "nginx:alpine|80:8080",
		},
		{
			name:  "multiple ports sorted",
			image: "nginx:alpine",
			ports: map[string]string{
				"443": "8443",
				"80":  "8080",
			},
			expected: "nginx:alpine|443:8443,80:8080",
		},
		{
			name:  "ports maintain sort order",
			image: "myapp:latest",
			ports: map[string]string{
				"9000": "19000",
				"3000": "13000",
				"8080": "18080",
			},
			expected: "myapp:latest|3000:13000,8080:18080,9000:19000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NewContainerKey(tt.image, tt.ports)
			if result != tt.expected {
				t.Errorf("NewContainerKey(%q, %v) = %q, want %q", tt.image, tt.ports, result, tt.expected)
			}
		})
	}
}

func TestContainerKey_String(t *testing.T) {
	key := ContainerKey("nginx:alpine|80:8080")
	if key.String() != "nginx:alpine|80:8080" {
		t.Errorf("String() = %q, want %q", key.String(), "nginx:alpine|80:8080")
	}
}

func TestContainerKey_Image(t *testing.T) {
	tests := []struct {
		key      ContainerKey
		expected string
	}{
		{"nginx:alpine|80:8080", "nginx:alpine"},
		{"nginx:alpine|", "nginx:alpine"},
		{"myapp:v1.0|3000:13000,8080:18080", "myapp:v1.0"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(string(tt.key), func(t *testing.T) {
			result := tt.key.Image()
			if result != tt.expected {
				t.Errorf("Image() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestContainerInfo_IsConfigurable(t *testing.T) {
	t.Run("label configured - not configurable", func(t *testing.T) {
		c := &ContainerInfo{HasLabelConfig: true}
		if c.IsConfigurable() {
			t.Error("expected IsConfigurable() = false for label-configured container")
		}
	})

	t.Run("not label configured - configurable", func(t *testing.T) {
		c := &ContainerInfo{HasLabelConfig: false}
		if !c.IsConfigurable() {
			t.Error("expected IsConfigurable() = true for non-label-configured container")
		}
	})
}

func TestContainerInfo_IsEnabled(t *testing.T) {
	tests := []struct {
		name            string
		hasLabelConfig  bool
		hasStoredConfig bool
		expected        bool
	}{
		{"neither", false, false, false},
		{"label only", true, false, true},
		{"stored only", false, true, true},
		{"both", true, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &ContainerInfo{
				HasLabelConfig:  tt.hasLabelConfig,
				HasStoredConfig: tt.hasStoredConfig,
			}
			if c.IsEnabled() != tt.expected {
				t.Errorf("IsEnabled() = %v, want %v", c.IsEnabled(), tt.expected)
			}
		})
	}
}
