package agent

import "testing"

func TestToolPortAndService(t *testing.T) {
	for tool, want := range map[string]struct {
		port int
		svc  string
	}{
		"":          {CollabPort, ToolPaperclip},
		"paperclip": {CollabPort, ToolPaperclip},
		"buzz":      {BuzzPort, ToolBuzz},
	} {
		if ToolPort(tool) != want.port || ToolService(tool) != want.svc {
			t.Errorf("%q: port %d service %s", tool, ToolPort(tool), ToolService(tool))
		}
	}
}

func TestObjectNames(t *testing.T) {
	w := WorkloadName("coder")
	for got, want := range map[string]string{
		ConfigMapName("coder"):    w + "-config",
		AWSConfigMapName("coder"): w + "-aws",
		EnvSecretName("coder"):    w + "-env",
		FilesSecretName("coder"):  w + "-files",
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
	if (&Controller{}).Kind() == "" {
		t.Error("kind")
	}
}
