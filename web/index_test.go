package web

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
	b, err := Content.ReadFile("index.html")
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
	b, err := Content.ReadFile("index.html")
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
	b, err := Content.ReadFile("index.html")
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
	b, err := Content.ReadFile("index.html")
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
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`>Local VMs <span id="localVmCount"`, `>OCI Images<`, `id="localVmRows"`, `id="ociImageRows"`,
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
	b, err := Content.ReadFile("index.html")
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
	b, err := Content.ReadFile("index.html")
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

// The Prepare VM for Jamf panel lost its intro paragraph: the workflow is now
// explained by the Auto enroll dropdowns, not by a base-VM-keeps-a-profile note.
func TestPrepareVMForJamfHasNoStaleIntro(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, unwanted := range []string{
		"Do not install or enroll the base VM",
		"Install the profile separately inside each clone",
	} {
		if strings.Contains(html, unwanted) {
			t.Errorf("dashboard still has the removed intro text %q", unwanted)
		}
	}
}

func TestDashboardKeepsMdmCopyDisabledWhileInFlight(t *testing.T) {
	b, err := Content.ReadFile("index.html")
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
	b, err := Content.ReadFile("index.html")
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
	b, err := Content.ReadFile("index.html")
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
	b, err := Content.ReadFile("index.html")
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
	b, err := Content.ReadFile("index.html")
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
	b, err := Content.ReadFile("index.html")
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
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`/api/" + kind`,
		`New VM starts are deferred while pressure is critical`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing memory safeguard %q", want)
		}
	}
}

func TestMemoryRecoveryActionsOnlyEnableForRunningVMs(t *testing.T) {
	b, err := Content.ReadFile("index.html")
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
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, requiredID := range []string{
		`id="terminalModal"`,
		`id="terminalCommand"`,
		`id="terminalSudoPassword"`,
		`id="terminalConsole"`,
		`id="noGraphics"`,
		`id="noAudio"`,
	} {
		if !strings.Contains(html, requiredID) {
			t.Errorf("missing required element %s in index.html", requiredID)
		}
	}
	// Check for the four Jamf shortcut commands in the terminal modal (full paths)
	for _, cmd := range []string{
		"sudo /usr/local/bin/jamf manage",
		"sudo /usr/local/bin/jamf recon",
		"sudo /usr/local/bin/jamf policy",
		"sudo /usr/local/bin/jamf checkJSSConnection",
	} {
		if !strings.Contains(html, cmd) {
			t.Errorf("missing Jamf command shortcut %q in terminal modal", cmd)
		}
	}
}

func TestUpdateBannersAndOptOut(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`id="checkTartUpdates"`, `disableTartUpdateCheck: !chk("checkTartUpdates")`,
		`id="checkOvenUpdates"`, `disableOvenUpdateCheck: !chk("checkOvenUpdates")`,
		`id="tartGithubLink"`, `href="https://github.com/openai/tart/releases"`,
		`id="ovenGithubLink"`, `href="https://github.com/vbnin/tart-oven/releases"`,
		`function renderUpdateBanners(`, `renderUpdateBanners(state.updates`,
		`/api/updates/dismiss`, `expandPanel("panel-tart-settings")`, `id="updateTartBtn"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

func TestHostnameControls(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`id="createHostname"`, `id="createHostnameFromName"`, `id="createHostnameField"`,
		`id="editVmHostname"`, `id="editVmHostnameFromName"`,
		`hostnameFromName: mode === "clone"`, `placeholder="My_VM-$AUTONUM"`, `Headless mode`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, unwanted := range []string{`value="macOS-Overview-"`, `Run scheduled VMs headless`, `id="checkForUpdates"`} {
		if strings.Contains(html, unwanted) {
			t.Errorf("index.html still has %q", unwanted)
		}
	}
}

func TestHeaderVersionAndLocalVMCount(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`<img class="app-icon" src="/icon.png"`, `<link rel="icon" type="image/png" sizes="512x512" href="/icon.png">`, `<span class="app-title">Tart Oven</span><span id="appVersion"`, `Local VMs <span id="localVmCount"`,
		`el("appVersion").textContent = "v" + state.version`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if strings.Contains(html, `" VMs";`+"\n  renderHeaderStats") {
		t.Error("VM count still rendered in the header")
	}
}

func TestRunWithArgumentsModal(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`id="runArgsModal"`, `id="runArgsOther"`, `id="runArgsPreview"`, `id="runArgsRunBtn"`,
		`data-arg="--vnc"`, `data-arg="--vnc-experimental"`, `data-arg="--no-graphics"`, `data-arg="@shared"`,
		`openRunArgsModal(`, `>Run with arguments</button>`, `JSON.stringify({ name, override })`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if strings.Contains(html, ">Run headless</button>") || strings.Contains(html, "function runHeadless") {
		t.Error("Run headless action is still present")
	}
}

func TestIPSWPickerUnsavedGuardAndPressureCase(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`id="ipswSource"`, `id="ipswShowBeta"`, `id="ipswBrowseBtn"`, `id="fromIpsw"`, `/api/ipsw/sources`, `/api/ipsw/choose-file`,
		`id="unsavedModal"`, `id="unsavedSaveBtn"`, `id="unsavedDiscardBtn"`, `id="unsavedStayBtn"`,
		`function isConfigDirty(`, `addEventListener("beforeunload"`, `function showTab(id, force)`,
		`sample.memoryPressure.charAt(0).toUpperCase()`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

func TestNameTemplateAndEnrollProfileControls(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`id="createNameTemplate"`, `placeholder="My_VM-$AUTONUM"`, `id="nameHelpBtn"`, `<template id="nameHelpContent">`, `<dt>$RAND8</dt>`, `<dt>$AUTONUM</dt>`,
		`nameTemplate: str("createNameTemplate")`,
		`id="createAutoEnrollProfile"`, `id="editVmAutoEnrollProfile"`, `function syncEnrollProfileSelects(`,
		`autoEnrollProfile: mode === "clone"`, `payload.autoEnrollProfile = choice`,
		`<h2>Prepare VM for Jamf</h2>`, `Start a VM first`, `This script prepares a base VM to support the Auto Enrollment VM feature on new VMs cloned from it.`,
		`Install Tart guest agent (or enable key-based SSH access)`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, unwanted := range []string{
		`id="createPrefix"`, `id="createAutoEnroll"`, `id="editVmAutoEnroll"`, `Prepare base VM for Jamf`, `Prepare one running base VM`,
		`Start the base VM first`, `Deploy an enrollment profile on the VM's desktop`, `readinessPill("Profile"`,
	} {
		if strings.Contains(html, unwanted) {
			t.Errorf("index.html still has %q", unwanted)
		}
	}
}

func TestHelpersLiveInQuestionMarkTips(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)

	// Field helpers moved into ? tips. Only two <small> remain, and both are
	// status text that JavaScript fills in.
	smalls := regexp.MustCompile(`<small[^>]*>`).FindAllString(html, -1)
	if len(smalls) != 2 || !strings.Contains(strings.Join(smalls, " "), `id="ipswStatus"`) || !strings.Contains(strings.Join(smalls, " "), `id="launchAtBootStatus"`) {
		t.Errorf("unexpected <small> helpers left: %v", smalls)
	}
	tips := strings.Count(html, `class="help-tip"`)
	if tips < 20 {
		t.Errorf("only %d ? tips", tips)
	}
	for _, want := range []string{
		`data-help="How often the scheduler acts"`, `data-help="Quote values with spaces, e.g. --dir=&quot;~/My Shared Folder&quot;"`,
		`id="helpPopover"`, `function openHelp(`, `function closeHelp(`, `e.target.closest(".help-tip")`,
		// Enable Auto-Enrollment Capabilities: requirements in a tip, first and last sentence kept.
		`id="prepRequirementsBtn"`, `<template id="prepRequirementsHelp">`, `<h4>Before you run the script</h4>`,
		`<p class="muted">Generate and deploy Jamf enrollment profiles on VMs.</p>`,
		`Keep Screen Sharing open to the VM while this script runs`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	// The requirements are no longer printed under the title.
	if strings.Contains(html, "Prior to execute this script") {
		t.Error("requirements intro is still printed below the title")
	}
	if strings.Contains(html, `id="nameHelpPopover"`) {
		t.Error("the old per-field name popover is still present")
	}
}

func TestHeaderBrandingLabelsAndRenameTip(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`font-family: "Helvetica Neue", Helvetica, Arial, sans-serif;`, `font-weight: 700;`, `font-size: 26px;`,
		`<label>VM storage path<button`, `<label>VM shared directory<button`, `<th data-sort="remaining">Timer</th>`,
		`<label>Rename to<button`, `data-help-src="nameHelpContent" data-help-note="Leave blank to keep the current name."`,
		`renamedTo = d.name || newName`, `data-help-note="Leave blank to use $RAND8."`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, unwanted := range []string{"🖥️ Tart Oven", "Time remaining", "Shared dir (host_resources)", "VM storage path (TART_HOME)"} {
		if strings.Contains(html, unwanted) {
			t.Errorf("index.html still has %q", unwanted)
		}
	}
}

func TestSecuritySettingsAndLoginControls(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`id="panel-server-settings"`, `id="authGenerateBtn"`, `id="authRevokeBtn"`, `id="authLogoutBtn"`,
		`id="authTokenReveal"`, `id="authCopyBtn"`, `id="tlsEnabled"`, `id="tlsCertPath"`, `id="tlsKeyPath"`,
		`id="tlsSelfSignedBtn"`, `id="loginModal"`, `id="loginToken"`, `id="loginBtn"`,
		`tlsEnabled: chk("tlsEnabled")`, `function submitLogin`, `function showLogin`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("security UI missing %q", want)
		}
	}
}
