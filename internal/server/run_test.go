package server

import "testing"

func TestRunWantsNoGraphics(t *testing.T) {
	testCases := []struct {
		name          string
		trigger       string
		headless      bool
		cfgNoGraphics bool
		extra         []string
		want          bool
	}{
		{"manual run, not headless", "manual", false, true, nil, false},
		{"manual run, headless", "manual", true, true, nil, true},
		{"scheduler run, config on", "scheduler", false, true, nil, true},
		{"scheduler run, config off", "scheduler", false, false, nil, false},
		{"explicit --no-graphics wins", "manual", false, false, []string{"--no-graphics"}, true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := runWantsNoGraphics(tc.trigger, tc.headless, tc.cfgNoGraphics, tc.extra)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunWantsNoAudio(t *testing.T) {
	testCases := []struct {
		name       string
		trigger    string
		cfgNoAudio bool
		extra      []string
		want       bool
	}{
		{"manual run, config on", "manual", true, nil, false},
		{"scheduler run, config on", "scheduler", true, nil, true},
		{"scheduler run, config off", "scheduler", false, nil, false},
		{"explicit --no-audio wins", "manual", false, []string{"--no-audio"}, true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := runWantsNoAudio(tc.trigger, tc.cfgNoAudio, tc.extra)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTakeAutoEnroll(t *testing.T) {
	testCases := []struct {
		name        string
		autoEnroll  bool
		profile     string
		mdmEnrolled bool
		wantRun     bool
		wantProfile string
	}{
		{"not set", false, "", false, false, ""},
		{"set, not enrolled", true, "", false, true, ""},
		{"set with a profile", true, "prod", false, true, "prod"},
		{"set, already enrolled", true, "prod", true, false, "prod"},
		{"already cleared", false, "", true, false, ""},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			vm := &VM{AutoEnroll: tc.autoEnroll, AutoEnrollProfile: tc.profile, MDMEnrolled: tc.mdmEnrolled}
			gotProfile, gotRun := takeAutoEnroll(vm)
			if gotRun != tc.wantRun || gotProfile != tc.wantProfile {
				t.Errorf("got (%q, %v), want (%q, %v)", gotProfile, gotRun, tc.wantProfile, tc.wantRun)
			}
			if vm.AutoEnroll || (tc.autoEnroll && vm.AutoEnrollProfile != "") {
				t.Errorf("one-shot not cleared: %+v", vm)
			}
		})
	}
}

func TestApplyAutoEnrollChoice(t *testing.T) {
	cfg := Config{JamfProfiles: []JamfProfile{{ID: "prod", Name: "Production"}, {ID: "qa", Name: "QA"}}}
	str := func(s string) *string { return &s }
	yes, no := true, false

	vm := &VM{}
	if err := applyAutoEnrollChoice(vm, cfg, nil, str("qa")); err != nil || !vm.AutoEnroll || vm.AutoEnrollProfile != "qa" {
		t.Fatalf("choose qa: %v %+v", err, vm)
	}
	if err := applyAutoEnrollChoice(vm, cfg, nil, str("prod")); err != nil || vm.AutoEnrollProfile != "prod" {
		t.Fatalf("switch to prod: %v %+v", err, vm)
	}
	if err := applyAutoEnrollChoice(vm, cfg, nil, str("nope")); err == nil || vm.AutoEnrollProfile != "prod" {
		t.Fatalf("unknown profile accepted or changed the VM: %v %+v", err, vm)
	}
	if err := applyAutoEnrollChoice(vm, cfg, nil, nil); err != nil || !vm.AutoEnroll || vm.AutoEnrollProfile != "prod" {
		t.Fatalf("omitted fields must leave it alone: %v %+v", err, vm)
	}
	if err := applyAutoEnrollChoice(vm, cfg, nil, str("")); err != nil || vm.AutoEnroll || vm.AutoEnrollProfile != "" {
		t.Fatalf("empty profile turns it off: %v %+v", err, vm)
	}
	// Older API callers use the plain flag.
	if err := applyAutoEnrollChoice(vm, cfg, &yes, nil); err != nil || !vm.AutoEnroll || vm.AutoEnrollProfile != "" {
		t.Fatalf("flag on: %v %+v", err, vm)
	}
	vm.AutoEnrollProfile = "qa"
	if err := applyAutoEnrollChoice(vm, cfg, &no, nil); err != nil || vm.AutoEnroll || vm.AutoEnrollProfile != "" {
		t.Fatalf("flag off clears the profile: %v %+v", err, vm)
	}
}

func TestJamfProfileByID(t *testing.T) {
	cfg := Config{JamfProfiles: []JamfProfile{{ID: "a", Name: "A"}}}
	if p, ok := jamfProfileByID(cfg, "a"); !ok || p.Name != "A" {
		t.Fatalf("found = %v %+v", ok, p)
	}
	if _, ok := jamfProfileByID(cfg, "b"); ok {
		t.Fatal("found a profile that doesn't exist")
	}
}

func TestValidateRunOverride(t *testing.T) {
	if err := validateRunOverride(nil); err != nil {
		t.Fatal(err)
	}
	if err := validateRunOverride(&runOverride{Args: []string{"--vnc", "--net-softnet-expose", "2222:22"}, Network: "shared"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []*runOverride{
		{Network: "bridged-ish"},
		{Args: []string{"--a\nb"}},
		{Args: make([]string, maxOverrideArgs+1)},
	} {
		if validateRunOverride(bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestHasNetArg(t *testing.T) {
	for _, args := range [][]string{{"--net-host"}, {"--net-softnet"}, {"--net-bridged=en0"}, {"--vnc", "--net-bridged", "en1"}} {
		if !hasNetArg(args) {
			t.Errorf("hasNetArg(%v) = false", args)
		}
	}
	if hasNetArg([]string{"--vnc", "--no-audio"}) {
		t.Error("hasNetArg without network args = true")
	}
}
