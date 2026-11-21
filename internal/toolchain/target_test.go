package toolchain

import (
	"strings"
	"testing"
)

// TestParseTargetTable is the executable spec for target classification. Every
// case names its expected *cause* (Class, GOOS, Native, Triple), not just
// "did it error": a classifier that returns ClassHost for a firmware triple
// would otherwise pass on a bare error check.
func TestParseTargetTable(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		goos       string
		goarch     string
		wantClass  Class
		wantGOOS   string
		wantNative bool
		wantTriple string
		wantErr    string
	}{
		{"default is native", "", "darwin", "arm64", ClassHost, "darwin", true, "", ""},
		{"native GOOS", "darwin", "darwin", "arm64", ClassHost, "darwin", true, "", ""},
		{"bare GOOS for another OS refused", "linux", "darwin", "arm64", ClassUnknown, "", false, "", "needs a full LLVM target triple"},
		{"unknown bare GOOS refused", "plan9", "darwin", "arm64", ClassUnknown, "", false, "", "not a known GOOS"},
		{"user example: x86_64-apple-darwin", "x86_64-apple-darwin", "darwin", "arm64", ClassHost, "darwin", false, "x86_64-apple-darwin", ""},
		{"same triple, native arch", "arm64-apple-darwin", "darwin", "arm64", ClassHost, "darwin", true, "arm64-apple-darwin", ""},
		{"user example: x86_64-unknown-linux-gnu", "x86_64-unknown-linux-gnu", "darwin", "arm64", ClassHost, "linux", false, "x86_64-unknown-linux-gnu", ""},
		{"linux triple on linux host is native", "x86_64-unknown-linux-gnu", "linux", "amd64", ClassHost, "linux", true, "x86_64-unknown-linux-gnu", ""},
		{"user example: thumbv7em-none-eabi", "thumbv7em-none-eabi", "darwin", "arm64", ClassBareMetal, "none", false, "thumbv7em-none-eabi", ""},
		{"arm-none-eabihf", "arm-none-eabihf", "linux", "amd64", ClassBareMetal, "none", false, "arm-none-eabihf", ""},
		{"4-field bare metal", "thumbv7em-none-none-eabi", "darwin", "arm64", ClassBareMetal, "none", false, "thumbv7em-none-none-eabi", ""},
		{"riscv32 bare metal", "riscv32imc-unknown-none-elf", "darwin", "arm64", ClassBareMetal, "none", false, "riscv32imc-unknown-none-elf", ""},
		{"hosted ARM 3-field shorthand", "arm-linux-gnueabihf", "darwin", "arm64", ClassHost, "linux", false, "arm-linux-gnueabihf", ""},
		{"unknown OS fails closed", "wasm32-unknown-unknown", "darwin", "arm64", ClassUnknown, "", false, "", "not in the closed set"},
		{"2-field triple refused", "x86_64-unknown", "darwin", "arm64", ClassUnknown, "", false, "", "3 or 4 fields"},
		{"5-field triple refused", "a-b-c-d-e", "darwin", "arm64", ClassUnknown, "", false, "", "3 or 4 fields"},
		{"empty field refused", "x86_64--linux-gnu", "darwin", "arm64", ClassUnknown, "", false, "", "empty triple field"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseTarget(c.raw, c.goos, c.goarch)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseTarget(%q) must fail, got %+v", c.raw, got)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error %q does not name the cause %q", err.Error(), c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTarget(%q) unexpected error: %v", c.raw, err)
			}
			if got.Class != c.wantClass || got.GOOS != c.wantGOOS || got.Native != c.wantNative || got.Triple != c.wantTriple {
				t.Fatalf("ParseTarget(%q) = {class:%s goos:%q native:%v triple:%q}, want {class:%s goos:%q native:%v triple:%q}",
					c.raw, got.Class, got.GOOS, got.Native, got.Triple, c.wantClass, c.wantGOOS, c.wantNative, c.wantTriple)
			}
		})
	}
}

// TestHostedOSClosedSetMutation proves ParseTarget actually consults the
// tables. Deleting an entry must turn a previously-valid target into a
// fail-closed refusal; if the classifier hard-coded "linux" somewhere, this
// goes red.
func TestHostedOSClosedSetMutation(t *testing.T) {
	orig := hostedOS["linux"]
	delete(hostedOS, "linux")
	defer func() { hostedOS["linux"] = orig }()

	_, err := ParseTarget("x86_64-unknown-linux-gnu", "darwin", "arm64")
	if err == nil {
		t.Fatal("with hostedOS[\"linux\"] removed the triple must be refused")
	}
	if !strings.Contains(err.Error(), "not in the closed set") {
		t.Fatalf("refusal must name the closed set, got %q", err.Error())
	}
	// The bare-GOOS branch must consult the same table, not a second copy.
	if _, err := ParseTarget("linux", "linux", "amd64"); err == nil {
		t.Fatal("bare GOOS \"linux\" must also be refused when the table loses its entry")
	}
}

// TestBareMetalOSClosedSetMutation is the hardware-side twin: if "none" stops
// being recognised, a firmware triple must NOT silently fall through to host.
func TestBareMetalOSClosedSetMutation(t *testing.T) {
	saved := bareMetalOS["none"]
	delete(bareMetalOS, "none")
	defer func() { bareMetalOS["none"] = saved }()

	got, err := ParseTarget("thumbv7em-none-eabi", "darwin", "arm64")
	if err == nil {
		t.Fatalf("with bareMetalOS[\"none\"] removed the triple must be refused, got %s (class %s)",
			got.Raw, got.Class)
	}
	if got.Class == ClassHost {
		t.Fatal("a firmware triple must never degrade to ClassHost")
	}
	if !strings.Contains(err.Error(), "not in the closed set") {
		t.Fatalf("refusal must name the closed set, got %q", err.Error())
	}
}

// TestClangTargetArgs pins that native builds get no -target (Apple's driver
// changes sysroot/linker choice when it is forced) while cross builds do.
func TestClangTargetArgs(t *testing.T) {
	native, err := ParseTarget("", "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if a := native.ClangTargetArgs(); len(a) != 0 {
		t.Fatalf("native build must not inject -target, got %v", a)
	}
	cross, err := ParseTarget("x86_64-unknown-linux-gnu", "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-target", "x86_64-unknown-linux-gnu"}
	got := cross.ClangTargetArgs()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("cross ClangTargetArgs() = %v, want %v", got, want)
	}
}
