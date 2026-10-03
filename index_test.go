package main

import (
	"regexp"
	"strings"
	"testing"
)

func sourceSection(t *testing.T, source, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(source, startMarker)
	if start < 0 {
		t.Fatalf("missing section start %q", startMarker)
	}
	end := strings.Index(source[start+len(startMarker):], endMarker)
	if end < 0 {
		t.Fatalf("missing section end %q after %q", endMarker, startMarker)
	}
	return source[start : start+len(startMarker)+end]
}

func TestDashboardContainsJamfProfileControls(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`id="jamfProfileRows"`, `id="addJamfProfileBtn"`, `id="saveJamfProfileBtn"`,
		`id="jamfBaseUrl"`, `id="jamfInvitationCode"`, `id="mdmProfileSelect"`,
		`id="mdmTarget"`,
		`id="copyMdmBtn"`, `/api/vm/mdm-profile`, `~/Desktop/mdm_enroll.mobileconfig`,
		`placeholder="https://tenant.jamfcloud.com"`, `Enter the value after invitation=, not the full URL`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// The per-call SSH override fields were removed deliberately: this panel
	// always uses the target VM's own SSH credentials (or the Configuration
	// defaults), never a separately-typed override.
	for _, unwanted := range []string{`id="mdmSshUser"`, `id="mdmSshPassword"`} {
		if strings.Contains(html, unwanted) {
			t.Errorf("dashboard still has removed control %q", unwanted)
		}
	}
}

func TestDashboardContainsPrepGoldenImageControls(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`id="prepTarget"`, `id="prepGoldenBtn"`, `id="prepGoldenStatus"`,
		`/api/vm/prep-golden-image`, `Enable Auto-Enrollment Capabilities on Base VM`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}

func TestDashboardRestoresGlobalSSHControlsAndSafeGuideBinding(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	sshConfig := sourceSection(t, html, `<h2>SSH &amp; Commands</h2>`, `<h2>Server Settings</h2>`)
	for _, want := range []string{
		`id="sshUser"`, `id="sshPassword"`, `for="sshUser"`, `for="sshPassword"`,
		`Default SSH username`, `Default SSH password`,
	} {
		if !strings.Contains(sshConfig, want) {
			t.Errorf("SSH configuration missing %q", want)
		}
	}
	for _, want := range []string{`function bindSshGuideInputs`, `if (el) el.addEventListener("input", updateSshGuide)`, `bindSshGuideInputs();`} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard startup is not protected from a missing SSH guide input: missing %q", want)
		}
	}
}

func TestEveryLiteralDOMIDReferenceHasAnElement(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	ids := make(map[string]bool)
	for _, match := range regexp.MustCompile(`\bid="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		ids[match[1]] = true
	}
	for _, match := range regexp.MustCompile(`(?:getElementById|\bel)\("([^"]+)"\)`).FindAllStringSubmatch(html, -1) {
		if !ids[match[1]] {
			t.Errorf("JavaScript references missing DOM id %q", match[1])
		}
	}
}

func TestDashboardSeparatesLocalVMsAndOCIImages(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`>Local VMs<`, `>OCI Images<`, `id="localVmRows"`, `id="ociImageRows"`,
		`Image location`, `Cached size`, `Virtual disk`, `Last accessed`,
		`function isOCI`, `function cloneFromOCI`, `function renderOCIImages`,
		`id="excludeOciFromScheduler"`, `excludeOciFromScheduler: document.getElementById("excludeOciFromScheduler").checked`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing OCI separation behavior %q", want)
		}
	}
}

func TestOCIImagesAreCloneOnlyAndHiddenByRunningFilter(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	ociRenderer := sourceSection(t, html, `function renderOCIImages`, `function renderTable`)
	for _, want := range []string{
		`runningOnly`, `ociPanel.classList.toggle("hidden", runningOnly)`,
		`cloneFromOCI(`, `>Clone</button>`,
	} {
		if !strings.Contains(ociRenderer, want) {
			t.Errorf("OCI renderer missing %q", want)
		}
	}
	for _, forbidden := range []string{`act('run'`, `act('stop'`, `openVNC(`, `openEditModal(`} {
		if strings.Contains(ociRenderer, forbidden) {
			t.Errorf("OCI renderer exposes local-only action %q", forbidden)
		}
	}
	clone := sourceSection(t, html, `function cloneFromOCI`, `function renderOCIImages`)
	for _, want := range []string{
		`showTab("vmm")`, `setCreateMode("clone")`, `cloneSource.value = name`,
	} {
		if !strings.Contains(clone, want) {
			t.Errorf("OCI clone handoff missing %q", want)
		}
	}
}

func TestManagementActionsOnlyTargetLocalVMs(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	section := sourceSection(t, string(b), `function fillMgmtSelects`, `function updateMdmCopyButton`)
	if !strings.Contains(section, `const local = vms.filter(v => !isOCI(v.source));`) {
		t.Fatal("VM management selectors do not filter edit/delete/MDM actions to local VMs")
	}
	// The Edit a VM panel was removed in v1.55-dev5; only check for running local filter
	if !strings.Contains(section, `const running = local.filter`) {
		t.Error("local-only management selector missing running VM filter")
	}
}

func TestDashboardExplainsPreparedBaseCloneWorkflow(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		"Do not install or enroll the base VM",
		"Install the profile separately inside each clone",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing Jamf base workflow guidance %q", want)
		}
	}
}

func TestDashboardKeepsMdmCopyDisabledWhileInFlight(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		"let mdmCopyInFlight = false;",
		"copyBtn.disabled = mdmCopyInFlight || !hasRunningVM;",
		"if (mdmCopyInFlight) return;",
		"mdmCopyInFlight = true;",
		"mdmCopyInFlight = false;\n    updateMdmCopyButton();",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing MDM copy in-flight guard %q", want)
		}
	}
}

func TestDashboardShowsSafeConfigValidationMessage(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`const errorText = res.ok ? "" : await res.text();`,
		`res.ok ? "saved ✓" : (errorText || "save failed")`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard does not display config validation response: missing %q", want)
		}
	}
}

func TestDashboardContainsPerformancePage(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`data-tab="performance"`, `id="tab-performance"`,
		`id="perfCpu"`, `id="perfMemory"`, `id="perfPressure"`,
		`id="perfSystemDisk"`, `id="perfVMDisk"`, `id="perfUptime"`, `id="perfUpdated"`,
		`id="cpuChart"`, `id="memoryChart"`, `id="diskCapacityChart"`, `id="diskIOChart"`,
		`/api/performance`, `function loadPerformance`, `function renderPerformance`,
		`function drawLineChart`, `function performanceColour`, `function formatBytes`, `function formatRate`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(html, `class="stat-label">CPU (5 min)</span>`) {
		t.Error("header still labels actual CPU utilization as a five-minute average")
	}
	if !strings.Contains(html, `class="stat-label">CPU</span>`) {
		t.Error("header missing CPU label")
	}
}

func TestPerformancePageUsesApprovedThresholds(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	colourFunction := sourceSection(t, string(b), `function performanceColour`, `function drawLineChart`)
	cpuBlock := sourceSection(t, colourFunction, `if (kind === "cpu")`, `if (kind === "disk")`)
	diskBlock := sourceSection(t, colourFunction, `if (kind === "disk")`, `if (kind === "pressure")`)
	pressureBlock := sourceSection(t, colourFunction, `if (kind === "pressure")`, `return "var(--green)"`)
	for _, want := range []string{`value > 95`, `value > 80`} {
		if !strings.Contains(cpuBlock, want) {
			t.Errorf("CPU block missing threshold %q", want)
		}
	}
	for _, want := range []string{`value > 90`, `value > 80`} {
		if !strings.Contains(diskBlock, want) {
			t.Errorf("disk block missing threshold %q", want)
		}
	}
	for _, want := range []string{`pressure === "critical"`, `pressure === "warning"`} {
		if !strings.Contains(pressureBlock, want) {
			t.Errorf("pressure block missing threshold %q", want)
		}
	}
	if strings.Contains(cpuBlock, `value > 90`) || strings.Contains(diskBlock, `value > 95`) {
		t.Error("critical thresholds are associated with the wrong metric")
	}
}

func TestPerformanceHistoryLoadsOnlyWhileVisible(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	showTab := sourceSection(t, html, `function showTab`, `// ---- performance ----`)
	render := sourceSection(t, html, `function render(state)`, `function renderTable`)
	for name, function := range map[string]string{"showTab": showTab, "render": render} {
		if strings.Count(function, `loadPerformance();`) != 1 {
			t.Errorf("%s must contain exactly one performance load", name)
		}
	}
	if !strings.Contains(showTab, `if (id === "performance") loadPerformance();`) {
		t.Error("showTab performance load is not scoped to opening Performance")
	}
	if !strings.Contains(render, `activeTab === "performance"`) {
		t.Error("SSE render performance load is not visibility guarded")
	}
	// A new sample only appears once a minute while SSE pushes arrive far more often,
	// so the render path must also gate on the sample timestamp advancing.
	if !strings.Contains(render, `sampleAt !== lastPerformanceSampleAt`) {
		t.Error("SSE render performance load is not gated on a new sample")
	}
}

func TestPerformancePressureLegendNamesEveryState(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	legend := sourceSection(t, string(b), `id="memoryPressureLegend"`, `</div>`)
	for _, state := range []string{"Normal", "Warning", "Critical"} {
		if !strings.Contains(legend, state) {
			t.Errorf("memory-pressure legend missing %q", state)
		}
	}
}

func TestDashboardContainsMemoryRecoveryActions(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`/api/" + kind`,
		`New VM starts are deferred while pressure is critical`,
		`id="editVmMemorySuggestion"`, `Lowering memory applies on the next boot`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing memory safeguard %q", want)
		}
	}
}

func TestMemoryRecoveryActionsOnlyEnableForRunningVMs(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	renderTable := sourceSection(t, string(b), `function renderTable`, `// keep "time remaining"`)
	for _, want := range []string{
		`const notRunning = vm.state !== "running" ? " disabled" : "";`,
		`const stopDisabled = busy;`,
		`act(\'stop\'`,
	} {
		if !strings.Contains(renderTable, want) {
			t.Errorf("running-only action guard missing %q", want)
		}
	}
}

// TestVMLookupClearsPreviousMemorySuggestion removed in v1.55-dev5: loadVMInfo
// function was removed when the Edit a VM panel was replaced with a modal.

func TestJamfCommandControlsAndSchedulerOptions(t *testing.T) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, requiredID := range []string{
		`id="cmdTabSshBtn"`,
		`id="cmdTabJamfBtn"`,
		`id="cmdTabSsh"`,
		`id="cmdTabJamf"`,
		`id="jamfCmdTarget"`,
		`id="jamfSudoPw"`,
		`id="jamfBtnPolicy"`,
		`id="jamfBtnRecon"`,
		`id="jamfBtnManage"`,
		`id="jamfEventName"`,
		`id="jamfBtnEvent"`,
		`id="noGraphics"`,
		`id="noAudio"`,
	} {
		if !strings.Contains(html, requiredID) {
			t.Errorf("missing required element %s in index.html", requiredID)
		}
	}
}
