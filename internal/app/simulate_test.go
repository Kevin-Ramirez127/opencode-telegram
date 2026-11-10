package app

import (
	"strings"
	"testing"
)

func TestSimulationScenariosUsePremadeConversations(t *testing.T) {
	a, ftg, _ := newIntegrationApp(t)
	server := newSimulationServer()
	defer server.Close()

	a.oc.SetEndpoint(server.URL(), "")
	a.rememberSession(server.session)
	simSettings, cleanup, err := newSimulationSettings()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	a.settings = simSettings

	scenarios := []struct {
		name string
		want string
	}{
		{"text", "Simulated OpenCode output"},
		{"done", "Session finished"},
		{"failed", "Simulated provider failure"},
		{"interrupted", "simulated interrupt"},
		{"question1", "Simulated question with 1 field(s)"},
		{"question2", "Simulated question with 2 field(s)"},
		{"question3", "Simulated question with 3 field(s)"},
		{"permission", "Simulated permission request"},
		{"retry", "retry #1"},
	}
	for _, scenario := range scenarios {
		before := len(ftg.allTexts())
		if err := a.runSimulationScenario(scenario.name, server); err != nil {
			t.Fatalf("%s: %v", scenario.name, err)
		}
		after := ftg.allTexts()
		if len(after) == before {
			t.Fatalf("%s sent no messages", scenario.name)
		}
		sent := strings.Join(after[before:], "\n")
		if !strings.Contains(strings.ToLower(sent), strings.ToLower(scenario.want)) {
			t.Fatalf("%s messages missing %q:\n%s", scenario.name, scenario.want, sent)
		}
	}
}
