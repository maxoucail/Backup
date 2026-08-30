package models

import "testing"

// EffectiveIntervalMinutes is the single place that resolves a device's
// backup interval - shared by the REST API (policy push, dashboard
// estimate) and the WS hub (overdue-on-reconnect check). A device override
// must win when present; the server default is only a fallback.
func TestEffectiveIntervalMinutesPrefersDeviceOverride(t *testing.T) {
	settings := &Settings{DefaultIntervalMinutes: 60}
	override := 15
	device := &Device{IntervalMinutes: &override}

	if got := EffectiveIntervalMinutes(device, settings); got != 15 {
		t.Fatalf("intervalle effectif = %d, attendu 15 (la valeur propre à l'appareil)", got)
	}
}

func TestEffectiveIntervalMinutesFallsBackToServerDefault(t *testing.T) {
	settings := &Settings{DefaultIntervalMinutes: 60}
	device := &Device{}

	if got := EffectiveIntervalMinutes(device, settings); got != 60 {
		t.Fatalf("intervalle effectif = %d, attendu 60 (le défaut du serveur)", got)
	}
}
