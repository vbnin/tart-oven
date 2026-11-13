package web

import (
	"os"
	"regexp"
	"strconv"
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
		`placeholder="https://tenant.jamfcloud.com"`, `id="jamfHowProBtn" data-help-src="jamfInvitationHelp"`,
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

// A saved Jamf server is either Pro (invitation code) or School (network and
// location IDs). The editor swaps the credential fields on a segmented control
// and the table gains a Type column.
func TestDashboardJamfProfileEditorSupportsProAndSchool(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`id="jamfKindProBtn"`, `id="jamfKindSchoolBtn"`, `id="jamfProfileKind"`,
		`id="jamfProFields"`, `id="jamfSchoolFields"`, `id="jamfNetwork"`, `id="jamfLocation"`,
		`id="jamfKindNote"`, `<th>Type</th>`, `<th>Base URL</th>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// "Jamf Pro" is no longer the only kind of server, so neither the column
	// nor the field may claim it.
	for _, unwanted := range []string{`<th>Jamf Pro Base URL</th>`, `<label>Jamf Pro base URL</label>`} {
		if strings.Contains(html, unwanted) {
			t.Errorf("dashboard still labels the base URL as Pro-only: %q", unwanted)
		}
	}
	// The secrets are write-only like the invitation code: password inputs, so
	// they are masked while typed and never prefilled.
	for _, id := range []string{"jamfNetwork", "jamfLocation"} {
		if !regexp.MustCompile(`<input[^>]*type="password"[^>]*id="` + id + `"`).MatchString(html) {
			t.Errorf("%s must be a password input", id)
		}
	}
	// The unload guard must actually be registered, or a mid-copy navigation is unprotected.
	if !strings.Contains(html, `window.addEventListener("beforeunload", warnBeforeUnload)`) {
		t.Error("warnBeforeUnload is not registered as the beforeunload listener")
	}
	// Every column has a matching cell: the empty-state row spans them all.
	if got := strings.Count(sourceSection(t, html, `<tbody id="jamfProfileRows">`, `</tbody>`), `colspan="5"`); got != 1 {
		t.Errorf("empty-state row should span the 5 columns, found %d colspan=5", got)
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
		`openCreateVmModal(`, `mode: "clone"`, `source: name`,
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
		`vmControlButtons(vm, n, !!vm.busy)`,
	} {
		if !strings.Contains(renderTable, want) {
			t.Errorf("running-only action guard missing %q", want)
		}
	}
	controls := sourceSection(t, string(b), `function vmControlButtons(`, `function renderTable(`)
	for _, want := range []string{
		`act(\'stop\'`, `(busy ? " disabled" : "")`, // Stop: only while an operation is in flight
		`act(\'run\'`, `suspendVM(\'`,
	} {
		if !strings.Contains(controls, want) {
			t.Errorf("vmControlButtons missing %q", want)
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
		`id="helpPopover"`, `function openHelp(`, `function closeHelp(`, `e.target.closest(".help-tip, .help-btn")`,
		// Enable Auto-Enrollment Capabilities: requirements in a tip, first and last sentence kept.
		`id="prepRequirementsBtn"`, `<template id="prepRequirementsHelp">`, `<h4>Before you run the script</h4>`,
		`<p class="muted">Auto enroll needs both steps: add your Jamf servers, then run the script once on the base VM. Clones get their own profile at first boot.</p>`,
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

func TestAgentAccessControls(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`id="agentEnabled"`, `id="agentMaxTtlMin"`, `id="agentTemplates"`, `id="agentTemplatesBtn"`, `id="agentTemplatesList"`, `id="agentTokenName"`,
		`id="agentTokenCreateBtn"`, `id="agentTokenList"`, `id="agentTokenReveal"`, `id="agentTokenCopyBtn"`,
		`agentEnabled: chk("agentEnabled")`, `agentMaxTtlMin: num("agentMaxTtlMin")`, `agentTemplates: agentTemplatesSelected.slice()`,
		`/api/auth/agent-tokens`, `vm.lease`, `.pill.agent`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("agent access UI missing %q", want)
		}
	}
}

// The new agent token must appear below the field that creates it.
func TestAgentTokenListIsBelowTheCreateField(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	create := strings.Index(html, `id="agentTokenCreateBtn"`)
	reveal := strings.Index(html, `id="agentTokenReveal"`)
	list := strings.Index(html, `id="agentTokenList"`)
	if create < 0 || reveal < 0 || list < 0 || !(create < reveal && reveal < list) {
		t.Errorf("want create field (%d) < token reveal (%d) < token list (%d)", create, reveal, list)
	}
}

// Agent settings live in their own panel, above Server Settings, and the panel
// is shown by a toggle in Server Settings (like the Jamf features).
func TestAgentAccessHasItsOwnPanelAboveServerSettings(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	panel := strings.Index(html, `id="panel-agent-access"`)
	server := strings.Index(html, `id="panel-server-settings"`)
	if panel < 0 || server < 0 || panel > server {
		t.Fatalf("want panel-agent-access (%d) above panel-server-settings (%d)", panel, server)
	}
	if !strings.Contains(html[panel:server], "<h2>Agentic AI access</h2>") {
		t.Error("the panel needs the title \"Agentic AI access\"")
	}
	for _, id := range []string{"agentEnabled", "agentMaxTtlMin", "agentTemplatesBtn", "agentTokenCreateBtn", "agentTokenList"} {
		if !strings.Contains(html[panel:server], `id="`+id+`"`) {
			t.Errorf("%s should be inside the Agentic AI access panel", id)
		}
		if strings.Contains(html[server:], `id="`+id+`"`) {
			t.Errorf("%s is still in Server Settings", id)
		}
	}
	// The toggle that shows the panel belongs to Add-Ons.
	addons := sourceSection(t, html, `id="panel-addons"`, `id="cfgSaveBar"`)
	if !strings.Contains(addons, `id="showAgentFeatures"`) {
		t.Error("Add-Ons needs the Display agentic AI features toggle")
	}
	for _, want := range []string{`showAgentFeatures: chk("showAgentFeatures")`, `applyAgentVisibility(state.config.showAgentFeatures === true)`, `.agent-off { display: none !important; }`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// Step 1 of the Setup Wizard offers the opt-in Jamf and agentic AI features.
func TestWizardStepOneOffersExtraFeatures(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	step1 := strings.Index(html, `id="wizardStep1"`)
	step2 := strings.Index(html, `id="wizardStep2"`)
	if step1 < 0 || step2 < step1 {
		t.Fatal("wizard step 1 not found")
	}
	for _, want := range []string{"Extra features", `id="wizardShowJamf"`, `id="wizardShowAgent"`, "onWizardFeatureChange()"} {
		if !strings.Contains(html[step1:step2], want) {
			t.Errorf("wizard step 1 is missing %q", want)
		}
	}
	for _, want := range []string{`id="wizardReviewFeatures"`, "function fillWizardFeatures", "wizardFeatureValues()"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(html, "A token is required to use this dashboard.") {
		t.Error("the \"token is required\" sentence should be gone")
	}
}

// The wizard's IPSW step can use a file on this Mac as well as the online list.
func TestWizardIPSWStepOffersALocalFile(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	start := strings.Index(html, `id="wizardIpswPanel"`)
	end := strings.Index(html, `id="wizardPullStatus"`)
	if start < 0 || end < start {
		t.Fatal("wizard IPSW panel not found")
	}
	panel := html[start:end]
	for _, want := range []string{`id="wizardIpswSelect"`, `id="wizardIpswPath"`, `id="wizardIpswBrowseBtn"`, `browseWizardIpsw(this)`, `Or a file on this Mac`} {
		if !strings.Contains(panel, want) {
			t.Errorf("wizard IPSW panel is missing %q", want)
		}
	}
	for _, want := range []string{"function wizardIpswSource", "/api/ipsw/choose-file"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// The OCI Images header has the pull button, progress and Cancel, but no Show logs
// button (the create panel in VM Management keeps its own).
func TestOCIImagesHeaderHasNoShowLogsButton(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	start := strings.Index(html, `id="ociPanel"`)
	end := strings.Index(html, `id="ociImageRows"`)
	if start < 0 || end < start {
		t.Fatal("OCI Images panel not found")
	}
	panel := html[start:end]
	if strings.Contains(panel, "Show logs") || strings.Contains(html, `id="pullShowLogs"`) {
		t.Error("the OCI Images section should not have a Show logs button")
	}
	for _, keep := range []string{`id="pullOciBtn"`, `id="pullProgress"`, `id="pullCancel"`} {
		if !strings.Contains(panel, keep) {
			t.Errorf("OCI Images header lost %s", keep)
		}
	}
	if !strings.Contains(html, `id="createShowLogs"`) {
		t.Error("the create panel's own Show logs button should stay")
	}
}

func TestTerminalIsARealTerminalLayout(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, id := range []string{
		`id="terminalUser"`, `id="terminalPrompt"`, `id="terminalStopBtn"`, `id="terminalScriptFile"`,
		`id="terminalScript"`, `id="terminalScriptRun"`, `Sending commands as:`,
	} {
		if !strings.Contains(html, id) {
			t.Errorf("terminal is missing %s", id)
		}
	}
	// The command line sits below the output, as in a real terminal.
	out := strings.Index(html, `id="terminalConsole"`)
	in := strings.Index(html, `id="terminalCommand"`)
	if out < 0 || in < 0 || in < out {
		t.Errorf("the command field (%d) must come after the output (%d)", in, out)
	}
	if !strings.Contains(html, `<textarea id="terminalCommand"`) {
		t.Error("the command field should be a textarea so a command can span lines")
	}
}

func TestCreateFormStaysInVMManagementAndOpensAsAModalFromTheDashboard(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	// The full form is always available in VM Management, with its progress controls.
	vmm := sourceSection(t, html, `id="tab-vmm"`, `id="panel-prep-jamf"`)
	for _, want := range []string{
		`id="createFormBody"`, `id="createBtn"`, `id="cloneSource"`, `id="createNameTemplate"`, `id="createRandMac"`,
		`id="provisioningEnabled"`, `id="createProgress"`, `id="createCancel"`, `id="createShowLogs"`,
	} {
		if !strings.Contains(vmm, want) {
			t.Errorf("VM Management lost %s", want)
		}
	}
	if strings.Contains(vmm, `+ New VM`) {
		t.Error("the form is inline again; the +New VM stand-in button should be gone")
	}
	// The Dashboard opens the same form in a window, by moving it, not copying it.
	modal := sourceSection(t, html, `id="createVmModal"`, `<!-- Terminal modal -->`)
	if !strings.Contains(modal, `id="createModalSlot"`) || strings.Contains(modal, `id="createBtn"`) {
		t.Error("the window should only hold a slot; the form must exist once")
	}
	dash := sourceSection(t, html, `id="tab-dashboard"`, `id="tab-performance"`)
	if !strings.Contains(dash, `openCreateVmModal()`) || strings.Contains(dash, `goToCreatePanel`) {
		t.Error("the Dashboard's + Create VM button should open the create window")
	}
	for _, want := range []string{`slot.appendChild(body)`, `panel.appendChild(body)`} {
		if !strings.Contains(html, want) {
			t.Errorf("the form is not moved in and out of the window: missing %q", want)
		}
	}
	if strings.Count(html, `id="createBtn"`) != 1 {
		t.Error("createBtn must exist exactly once")
	}
}

func TestEditVMTagsArePickedFromThoseInUse(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{`id="editVmTagBox"`, `id="editVmTagSuggest"`, `id="editVmTags"`, `function canonicalTag(`, `function tagUsage(`, `In use:`} {
		if !strings.Contains(html, want) {
			t.Errorf("tag picker is missing %q", want)
		}
	}
	if strings.Contains(html, `Tags (comma-separated)`) {
		t.Error("the free-text tag field should be replaced by the picker")
	}
}

func TestSnapshotsHaveAColumnAMenuEntryAndAWindow(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`<th data-sort="snapshots">Snapshots</th>`, `snapshotsCell(vm)`, `openSnapshotsModal(`, `>VM Snapshots</button>`,
		`id="snapshotsModal"`, `id="snapshotRows"`, `id="snapshotComment"`, `id="snapshotCreateBtn"`,
		`id="deleteVmModal"`, `name="deleteVmChoice"`, `/api/vm/snapshot/restore`, `/api/vm/snapshot/comment`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("snapshots UI is missing %q", want)
		}
	}
	// Snapshots are not listed as VMs on the dashboard: the window is the only place.
	if strings.Contains(sourceSection(t, html, `function renderTable(`, `// keep "time remaining"`), `vm.snapshots.map`) {
		t.Error("renderTable must not list snapshots as rows")
	}
}

func TestInternetHasAColumnAndADetailsRow(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`<th data-sort="internet">Internet</th>`, `internetCell(vm)`, `row("Internet", internet)`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("internet UI is missing %q", want)
		}
	}
	// The table has one cell per header; the loading row and the message row span them all.
	if got := strings.Count(html, `<td colspan="11" class="muted">`); got != 2 {
		t.Errorf("found %d full-width VM table rows spanning 11 columns, want 2", got)
	}
}

func TestSuspendIsPerVMWithASchedulerOverrideAndNoAudioIsGone(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	tart := sourceSection(t, html, `id="panel-tart-settings"`, `id="updateTartBtn"`)
	if strings.Contains(tart, `id="suspendable"`) {
		t.Error("the global Start VMs suspendable setting is gone: it is per VM now")
	}
	if !strings.Contains(tart, `id="autoCheckSeconds"`) {
		t.Error("the auto-check interval belongs in Tart Settings")
	}
	sched := sourceSection(t, html, `id="panel-scheduler"`, `id="panel-tart-settings"`)
	if !strings.Contains(sched, `id="schedulerSuspend"`) {
		t.Error("the scheduler section needs Suspend mode")
	}
	if strings.Contains(sched, `id="suspendable"`) || strings.Contains(sched, `id="noAudio"`) {
		t.Error("the scheduler section should have neither the old global suspendable toggle nor Disable audio")
	}
	// The per-VM setting is offered when editing, creating and cloning, and sent to the server.
	for _, want := range []string{
		`id="editVmStartSuspendable"`, `id="createStartSuspendable"`,
		`startSuspendable: document.getElementById("editVmStartSuspendable").checked`,
		`startSuspendable: document.getElementById("createStartSuspendable").checked`,
		`vm.startSuspendable`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("per-VM suspendable UI is missing %q", want)
		}
	}
	if strings.Contains(html, "noAudio") {
		t.Error("nothing should refer to the removed noAudio setting")
	}
}

func TestEditVMCanRemoveCustomSSHCredentials(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`id="editVmClearSshPassword"`, `id="editVmClearSshUser"`, `vm.sshPasswordSet`, `clearSshPassword`, `clearSshUser`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Edit VM can't remove custom credentials: missing %q", want)
		}
	}
}

func TestVMTablePinsNameAndActionsAndScrollsTheRest(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	table := sourceSection(t, html, `id="vmShell"`, `id="localVmRows"`)
	for _, want := range []string{
		`id="vmScroll"`, `class="vm-table"`, `id="vmCols"`,
		`<th class="pin-l" data-sort="name">Name</th>`, `<th class="pin-r">Actions</th>`,
	} {
		if !strings.Contains(table, want) {
			t.Errorf("VM table markup is missing %q", want)
		}
	}
	// One <col> per header: widths come from the colgroup (table-layout: fixed).
	cols := strings.Count(table, `<col data-col=`)
	heads := strings.Count(sourceSection(t, html, `id="localVmHead"`, `</thead>`), `<th`)
	if cols != heads || cols != 11 {
		t.Errorf("%d <col> elements for %d headers, want 11 of each", cols, heads)
	}
	row := sourceSection(t, html, `function renderTable(`, `// keep "time remaining"`)
	for _, want := range []string{`<td class="name pin-l">`, `<td class="acts pin-r">`, `updateVmScrollHints()`} {
		if !strings.Contains(row, want) {
			t.Errorf("renderTable no longer produces %q", want)
		}
	}
	// The pinned cells must be opaque (else scrolled columns show through), and an
	// open row menu must stay above the pinned cells of the rows below it.
	css := sourceSection(t, html, `.table-shell { position: relative; }`, `body.col-resizing {`)
	for _, want := range []string{
		`.vm-table tr.row-running:hover td.pin-r {` + "\n" + `    background-color: var(--panel);`,
		`.table-shell.can-left::before`, `.table-shell.can-right::after`,
		`var(--scroll-shade)`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("VM table CSS is missing %q", want)
		}
	}
	// Both themes define the gradient colour.
	if strings.Count(html, `--scroll-shade:`) != 2 {
		t.Error("--scroll-shade must be defined for the dark and the light theme")
	}
}

func TestDev2FeedbackMarkup(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	// Timer comes before Snapshots, in the header, the colgroup and the rows.
	head := sourceSection(t, html, `id="localVmHead"`, `</thead>`)
	if strings.Index(head, `data-sort="remaining"`) > strings.Index(head, `data-sort="snapshots"`) {
		t.Error("Timer must come before Snapshots in the header")
	}
	cols := sourceSection(t, html, `id="vmCols"`, `</colgroup>`)
	if strings.Index(cols, `data-col="remaining"`) > strings.Index(cols, `data-col="snapshots"`) {
		t.Error("Timer must come before Snapshots in the colgroup")
	}
	rows := sourceSection(t, html, `function renderTable(`, `// keep "time remaining"`)
	if strings.Index(rows, `data-stop=`) > strings.Index(rows, `snapshotsCell(vm)`) {
		t.Error("the Timer cell must come before the Snapshots cell")
	}
	// Actions is fitted to its buttons; the spare width goes to Info.
	for _, want := range []string{`fitActionsColumn();`, `const VM_COL_SPARE = "info";`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	// The scheduler toggle is called Suspend mode.
	sched := sourceSection(t, html, `id="panel-scheduler"`, `id="panel-tart-settings"`)
	if !strings.Contains(sched, `</span></span> Suspend mode<button type="button" class="help-tip"`) {
		t.Error("the scheduler toggle should read Suspend mode, with its ? tip")
	}
	// A tip inside a flex label must not shrink.
	if !strings.Contains(html, "flex: none; /* inside a flex label") {
		t.Error("button.help-tip must not shrink (flex: none)")
	}
	// Auto enroll gets a warning sign, not a ?, in both places.
	for _, field := range []string{`Auto enroll at first boot<button`, `Auto enroll at next boot<button`} {
		i := strings.Index(html, field)
		if i < 0 {
			t.Fatalf("missing %q", field)
		}
		tip := html[i : i+200]
		if !strings.Contains(tip, `class="help-tip warn-tip"`) || strings.Contains(tip, `>?</button>`) {
			t.Errorf("%s should carry the warning sign: %s", field, tip)
		}
	}
}

// People skipped the auto-enrollment script because it read as a footnote of the
// server list. The panel is two numbered steps in their own cards, and copying a
// profile by hand is a separate optional card after them, so it is not read as a
// step towards cloning.
func TestPrepareVMForJamfIsTwoNumberedStepsThenAnOptionalCopy(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	panel := sourceSection(t, html, `id="panel-prep-jamf"`, `id="panel-guest-agent"`)
	order := []string{
		`<section class="jamf-step" id="jamfStep1"`, `<span class="jamf-step-num" aria-hidden="true">1</span>`, `>Jamf Server Profiles</h3>`,
		`id="jamfProfileRows"`, `id="jamfProfileEditor"`, `</section>`,
		`<section class="jamf-step" id="jamfStep2"`, `<span class="jamf-step-num" aria-hidden="true">2</span>`, `>Enable Auto-Enrollment Capabilities on Base VM</h3>`,
		`Run it once on the base VM, before you clone it`, `id="prepTarget"`,
		`id="prepRequirementsBtn" data-help-src="prepRequirementsHelp" aria-expanded="false">1. Prerequisites</button>`,
		`id="checkCompatBtn">2. Check compatibility</button>`, `id="prepGoldenBtn">3. Run accessibility script</button>`,
		`id="prepReadiness"`, `</section>`,
		`<section class="jamf-step jamf-step-optional" id="jamfManualCopy"`, `Copy a Profile Manually (optional)`,
		`id="mdmTarget"`, `id="mdmProfileSelect"`, `id="copyMdmBtn"`, `id="mdmCopyStatus"`, `</section>`,
	}
	at := 0
	for _, want := range order {
		i := strings.Index(panel[at:], want)
		if i < 0 {
			t.Fatalf("Prepare VM for Jamf: %q missing or out of order", want)
		}
		at += i + len(want)
	}
	if strings.Count(panel, `class="jamf-step-num"`) != 2 {
		t.Error("exactly two numbered steps expected")
	}
	if strings.Contains(panel, "Deploy MDM Enrollment Profile") {
		t.Error("the manual copy is no longer presented as a step")
	}
}

// The Helper Guide is README.md rendered in the app, where headings get ids from
// the same rule as the slug function in index.html. A link to a renamed heading
// would silently go nowhere.
func TestReadmeInPageLinksPointAtHeadings(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	nonAlnum := regexp.MustCompile(`[^a-z0-9]+`)
	slug := func(s string) string { return strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(s), "-"), "-") }
	ids := map[string]bool{}
	inFence := false
	for _, line := range strings.Split(string(readme), "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if m := regexp.MustCompile(`^#{1,6} (.+)$`).FindStringSubmatch(line); m != nil && !inFence {
			ids[slug(m[1])] = true
		}
	}
	links := regexp.MustCompile(`\]\(#([^)]+)\)`).FindAllStringSubmatch(string(readme), -1)
	if len(links) == 0 {
		t.Fatal("no in-page links found; the test is not looking at the README")
	}
	for _, l := range links {
		if !ids[l[1]] {
			t.Errorf("README links to #%s, but no heading has that id", l[1])
		}
	}
}

// The pinned Actions cell must stay opaque in every row state (the Snapshots
// count showed through it on a hovered running row), is never tinted, and its
// buttons never shrink.
func TestPinnedActionsCellStaysOpaqueAndItsButtonsKeepTheirSize(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`.vm-table tr.row-running:hover td.pin-r {` + "\n" + `    background-color: var(--panel); background-image: none;`,
		`.vm-table td.acts .row-actions > * { flex: none; }`,
		`if (!table || !table.offsetParent) return;`,
		`new ResizeObserver(() => { fitActionsColumn(); updateVmScrollHints(); })`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, unwanted := range []string{"tr.row-running td.pin-r {", "tr:hover td.pin-r {", "tr.row-running:hover td.pin-r { background-image"} {
		if strings.Contains(html, unwanted) {
			t.Errorf("the Actions cell is tinted again: %q", unwanted)
		}
	}
}

// Settings put their switches in a group of their own, apart from the text fields,
// in every Configuration panel and in Guest provisioning. A master switch (one that
// turns the fields it sits with on) comes first.
func TestSettingsKeepSwitchesApartFromTextFields(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	if strings.Contains(html, `<div class="field"><label>&nbsp;</label><label class="radio"><span class="toggle">`) {
		t.Error("a switch still sits in a text-field grid behind an empty label")
	}
	for panel, ids := range map[string][]string{
		`id="panel-scheduler"`:       {"dailyEnabled", "excludeOciFromScheduler", "noGraphics", "schedulerSuspend"},
		`id="panel-tart-settings"`:   {"checkTartUpdates"},
		`id="panel-ssh-commands"`:    {"sshFallbackEnabled", "prioritizeSshShutdown"},
		`id="panel-server-settings"`: {"launchAtBoot", "checkOvenUpdates", "tlsEnabled"},
		`id="panel-addons"`:          {"showJamfFeatures", "showAgentFeatures"},
		`id="panel-agent-access"`:    {"agentEnabled"},
	} {
		section := sourceSection(t, html, panel, `<div class="panel" id="`)
		for _, id := range ids {
			i := strings.Index(section, `id="`+id+`"`)
			g := strings.LastIndex(section[:max(i, 0)], `<div class="toggle-group`)
			if i < 0 || g < 0 || strings.Contains(section[g:i], `<div class="grid">`) {
				t.Errorf("%s: switch %s is not in a toggle group", panel, id)
			}
		}
	}
	// Master switches first: agent access, HTTPS, guest provisioning.
	for _, pair := range [][2]string{
		{`id="agentEnabled"`, `id="agentMaxTtlMin"`},
		{`id="tlsEnabled"`, `id="tlsCertPath"`},
		{`id="provisioningEnabled"`, `id="provisioningFullName"`},
	} {
		if strings.Index(html, pair[0]) > strings.Index(html, pair[1]) {
			t.Errorf("master switch %s must come before %s", pair[0], pair[1])
		}
	}
	if !strings.Contains(html, `<div id="provisioningCreateSection" class="sub-card"`) {
		t.Error("Guest provisioning should be a card, like the Jamf steps")
	}
	for _, want := range []string{`<label>VM info refresh interval (s)<button`, `#vmInfoModalBody { display: flex; flex-direction: column; gap: 14px; max-height: 70vh; overflow-y: auto; padding-right: 12px;`, `.kv-section { border: 1px solid var(--border);`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, gone := range []string{"<label>Compatibility</label>", "Select a VM to check.", "Auto-check interval (seconds)"} {
		if strings.Contains(html, gone) {
			t.Errorf("%q should be gone", gone)
		}
	}
}

// The four ideas from the dev4 review: app preferences apart from server setup,
// fields greyed out while the switch they need is off, rarely changed fields in a
// collapsed Advanced section, and a save bar that follows the page when needed.
func TestConfigurationIdeasFromTheDev4Review(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	// The two extra-feature switches have a section of their own; launch at login
	// and the update check stay with the server.
	addons := sourceSection(t, html, `id="panel-addons"`, `id="cfgSaveBar"`)
	if !strings.Contains(addons, "<h2>Add-Ons</h2>") {
		t.Error("the Add-Ons section needs its title")
	}
	server := sourceSection(t, html, `id="panel-server-settings"`, `id="panel-addons"`)
	for _, id := range []string{"showJamfFeatures", "showAgentFeatures"} {
		if !strings.Contains(addons, `id="`+id+`"`) || strings.Contains(server, `id="`+id+`"`) {
			t.Errorf("%s belongs in Add-Ons", id)
		}
	}
	for _, id := range []string{"launchAtBoot", "checkOvenUpdates"} {
		if !strings.Contains(server, `id="`+id+`"`) || strings.Contains(addons, `id="`+id+`"`) {
			t.Errorf("%s belongs in Server Settings", id)
		}
	}
	if strings.Contains(html, "App Preferences") {
		t.Error("the App Preferences section is gone")
	}
	// Dependent fields and their switch.
	for field, sw := range map[string]string{"dailyStart": "dailyEnabled", "dailyStop": "dailyEnabled", "tlsCertPath": "tlsEnabled", "tlsKeyPath": "tlsEnabled", "agentMaxTtlMin": "agentEnabled", "agentTemplatesBtn": "agentEnabled"} {
		i := strings.Index(html, `id="`+field+`"`)
		f := strings.LastIndex(html[:i], `<div class="field"`)
		if i < 0 || f < 0 || !strings.Contains(html[f:i], `data-depends-on="`+sw+`"`) {
			t.Errorf("%s should depend on %s", field, sw)
		}
	}
	sched := sourceSection(t, html, `id="panel-scheduler"`, `<div class="panel" id="`)
	if strings.Index(sched, `id="dailyEnabled"`) > strings.Index(sched, `id="dailyStart"`) {
		t.Error("Limit scheduler to daily hours must come right before its time fields")
	}
	// Advanced sections.
	for panel, ids := range map[string][]string{
		`id="panel-tart-settings"`:   {"tartAppPath", "runArgs"},
		`id="panel-ssh-commands"`:    {"statusCommand"},
		`id="panel-server-settings"`: {"logPath"},
	} {
		section := sourceSection(t, html, panel, `<div class="panel" id="`)
		adv := strings.Index(section, `<details class="advanced">`)
		for _, id := range ids {
			if adv < 0 || strings.Index(section, `id="`+id+`"`) < adv {
				t.Errorf("%s: %s should be under Advanced", panel, id)
			}
		}
	}
	for _, want := range []string{`id="cfgSaveBar"`, `.save-bar.dirty { position: sticky; bottom: 0;`, `function syncSaveBar(`, `function syncDependentFields(`, "  syncDependentFields();\n  markConfigClean();"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// Auto enroll only works on an English guest: the prerequisites say so and
// Check compatibility shows the language.
func TestCompatibilityChecksTheGuestLanguage(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	prereq := sourceSection(t, html, `<template id="prepRequirementsHelp">`, `</template>`)
	if !strings.Contains(prereq, "English (any region") {
		t.Error("the prerequisites must say the VM has to be in English")
	}
	if !strings.Contains(html, `readinessPill("Language", d.language, "english", "other", d.languageCode)`) {
		t.Error("Check compatibility must show the Language pill")
	}
}

// dev6: every on/off checkbox is a toggle (the Agent templates picker is a list,
// not a switch, and keeps its checkboxes); Guest provisioning waits for its
// switch; the Jamf invitation ID tip has a copyable API recipe; the row menu
// opens from <body> so a scroll box can't crop it.
func TestDev6ConsistencyAndHelp(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, line := range strings.Split(html, "\n") {
		if strings.Contains(line, `<input type="checkbox"`) && !strings.Contains(line, `<span class="toggle"><input type="checkbox"`) && !strings.Contains(line, "multi-select-item") {
			t.Errorf("checkbox that is not a toggle: %.140s", strings.TrimSpace(line))
		}
	}
	if strings.Contains(html, "(--insecure)") {
		t.Error("no CLI flags in labels")
	}
	for _, id := range []string{"provisioningFullName", "provisioningUsername", "provisioningPassword", "provisioningAutoLogin", "provisioningRemoteLogin"} {
		i := strings.Index(html, `id="`+id+`"`)
		f := strings.LastIndex(html[:i], `data-depends-on="provisioningEnabled"`)
		if i < 0 || f < 0 || strings.Count(html[f:i], "<div") > 2 {
			t.Errorf("%s should wait for Provision this VM automatically", id)
		}
	}
	help := sourceSection(t, html, `<template id="jamfInvitationHelp">`, `</template>`)
	for _, want := range []string{"/api/v1/auth/token", "/JSSResource/computerinvitations/id/0", "<invitation_type>USER_INITIATED_URL</invitation_type>", "<expiration_date>Unlimited</expiration_date>", "string(//invitation)", "/enroll?invitation=", `onclick="copyPopoverCode()"`} {
		if !strings.Contains(strings.ReplaceAll(strings.ReplaceAll(help, "&lt;", "<"), "&gt;", ">"), want) {
			t.Errorf("invitation recipe is missing %q", want)
		}
	}
	// The DEFAULT invitation type and the multiple_users/uses fields made Jamf Cloud answer HTTP 500
	// (2026-11-07 live test); USER_INITIATED_URL with an empty account is the form that works.
	for _, bad := range []string{"<invitation_type>DEFAULT<", "multiple_users_allowed", "multiple_uses_allowed"} {
		if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(help, "&lt;", "<"), "&gt;", ">"), bad) {
			t.Errorf("invitation recipe must not send %q", bad)
		}
	}
	for _, want := range []string{"function clearPortaledMenus(", "document.body.appendChild(list);", ".row-menu-list.portal-open"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// The how-to for each Jamf type is a visible button next to the type selector, not a
// ? on a label: the invitation recipe for Pro, where to find the IDs for School.
func TestJamfHowToButtonsSitNextToTheTypeSelector(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	editor := sourceSection(t, html, `id="jamfProfileEditor"`, `id="saveJamfProfileBtn"`)
	sel := strings.Index(editor, `aria-label="Jamf server type"`)
	pro := strings.Index(editor, `id="jamfHowProBtn"`)
	school := strings.Index(editor, `id="jamfHowSchoolBtn"`)
	fields := strings.Index(editor, `id="jamfProfileName"`)
	if sel < 0 || pro < sel || school < sel || pro > fields || school > fields {
		t.Fatalf("the buttons must sit between the type selector and the fields (%d %d %d %d)", sel, pro, school, fields)
	}
	for _, want := range []string{
		`>How to get an enrollment invitation ID</button>`, `>How to find enrollment information</button>`,
		`data-help-src="jamfInvitationHelp"`, `data-help-src="jamfSchoolHelp"`,
	} {
		if !strings.Contains(editor, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(editor, `<label>Invitation ID<button`) {
		t.Error("the Invitation ID label no longer carries a ? tip")
	}
	school2 := sourceSection(t, html, `<template id="jamfSchoolHelp">`, `</template>`)
	for _, want := range []string{"Devices", "Enroll Devices", "Network ID", "Location ID"} {
		if !strings.Contains(school2, want) {
			t.Errorf("the Jamf School help should mention %q", want)
		}
	}
}

// Add-Ons sits right after Server Settings (the last panel before the save bar); the AI
// client instructions are a button that is available once an agent token exists.
func TestAddOnsFollowServerSettingsAndConnectIsAButton(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	server := strings.Index(html, `id="panel-server-settings"`)
	addons := strings.Index(html, `id="panel-addons"`)
	save := strings.Index(html, `id="cfgSaveBar"`)
	if !(server > 0 && server < addons && addons < save) {
		t.Fatalf("want Server Settings (%d), then Add-Ons (%d), then the save bar (%d)", server, addons, save)
	}
	for _, between := range []string{`id="panel-scheduler"`, `id="panel-tart-settings"`, `id="panel-agent-access"`} {
		if i := strings.Index(html, between); i > server && i < addons {
			t.Errorf("%s sits between Server Settings and Add-Ons", between)
		}
	}
	agent := sourceSection(t, html, `id="panel-agent-access"`, `id="panel-server-settings"`)
	if !strings.Contains(agent, `id="agentConnectBtn" class="ghost hidden"`) || !strings.Contains(agent, `>How to connect an AI client</button>`) {
		t.Error("the connect instructions need their own button, hidden until a token exists")
	}
	for _, want := range []string{`function syncAgentConnectButton(`, `el("agentConnectBtn").onclick = toggleAgentConnect;`, `function currentAgentTokenOnScreen(`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// The row menu lives in <body> while open (a scroll box can't crop it), so it must
// bring its own keyboard handling and stay inside short windows.
func TestRowMenuKeyboardAndWindowFit(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		"function rowMenuTop(", "evt.detail === 0", `list.setAttribute("role", "menu")`,
		`if (e.key === "Escape" || e.key === "Tab") { e.preventDefault(); close(); return; }`,
		`max-height: calc(100vh - 16px); overflow-y: auto;`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// The open row menu lives in <body> at z-index 30. A pinned Actions cell lifted above
// that (the old td.pin-r:has(.row-menu.open) { z-index: 40 }) painted over the top of
// the menu and hid its first items, most of all in Safari.
func TestNoTableLayerIsAboveTheOpenRowMenu(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	if strings.Contains(html, ":has(.row-menu.open) {") {
		t.Error("an open row's cell must not be lifted above the menu")
	}
	menu := regexp.MustCompile(`\.row-menu-list \{[^}]*z-index: (\d+)`).FindStringSubmatch(html)
	if menu == nil {
		t.Fatal("row-menu-list has no z-index")
	}
	menuZ, _ := strconv.Atoi(menu[1])
	for _, m := range regexp.MustCompile(`(?s)\.vm-table[^{}]*\{[^}]*z-index: (\d+)`).FindAllStringSubmatch(html, -1) {
		if z, _ := strconv.Atoi(m[1]); z >= menuZ {
			t.Errorf("a table layer has z-index %d, not below the menu's %d", z, menuZ)
		}
	}
}

// The bulk delete trash button sits left of "+ Create VM" and starts hidden.
func TestBulkDeleteButtonSitsLeftOfCreate(t *testing.T) {
	b, err := Content.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	if strings.Count(html, `id="bulkDeleteBtn"`) != 1 {
		t.Fatal("bulkDeleteBtn must exist exactly once")
	}
	trash := strings.Index(html, `id="bulkDeleteBtn"`)
	create := strings.Index(html, `onclick="openCreateVmModal()">+ Create VM`)
	if create < 0 || trash > create {
		t.Errorf("the trash button must come before \"+ Create VM\" in the Local VMs header")
	}
	if !strings.Contains(html[trash:trash+400], `class="danger hidden"`) {
		t.Error("the trash button must start hidden and use the red danger style")
	}
}
