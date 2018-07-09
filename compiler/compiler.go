package compiler

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"text/template"

	"github.com/gen0cide/gscript/compiler/computil"
	"github.com/gen0cide/gscript/compiler/obfuscator"
	"github.com/gen0cide/gscript/engine"
	gparser "github.com/robertkrimen/otto/parser"
	"github.com/uudashr/gopkgs"
	"golang.org/x/tools/imports"
)

var (
	errNoVM = errors.New("compiler has no VMs to process")
)

// Compiler is the primary type for building native binaries with gscript
type Compiler struct {
	// lock to prevent race conditions during compilation
	sync.RWMutex

	// array of VMs that will be bundled into this build
	vms []*GenesisVM

	// a map that places subsets of VMs into buckets according to their priority
	SortedVMs map[int][]*GenesisVM

	// logging object to be used
	Logger engine.Logger

	// source buffer used by the pre-compilation obfuscator
	sourceBuffer bytes.Buffer

	// a slice of unique priorities that can be found within this VMs bundled into this build
	UniqPriorities []int

	// configuration object for the compiler
	Options
}

// NewWithDefault returns a new compiler object with default options
func NewWithDefault() *Compiler {
	return &Compiler{
		Logger:         &engine.NullLogger{},
		Options:        DefaultOptions(),
		SortedVMs:      map[int][]*GenesisVM{},
		vms:            []*GenesisVM{},
		UniqPriorities: []int{},
	}
}

// NewWithOptions returns a new compiler object with custom options
func NewWithOptions(o Options) *Compiler {
	return &Compiler{
		Logger:         &engine.NullLogger{},
		Options:        o,
		SortedVMs:      map[int][]*GenesisVM{},
		vms:            []*GenesisVM{},
		UniqPriorities: []int{},
	}
}

// SetLogger overrides the logger for the compiler (defaults to an engine.NullLogger)
func (c *Compiler) SetLogger(l engine.Logger) {
	c.Logger = l
}

// AddScript attempts to create a virtual machine object based on the given parameter to be included in compilation
func (c *Compiler) AddScript(scriptPath string) error {
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		return fmt.Errorf("script cannot be located at %s", scriptPath)
	}
	srcBytes, err := ioutil.ReadFile(scriptPath)
	if err != nil {
		return err
	}
	fileName := filepath.Base(scriptPath)
	absPath, err := filepath.Abs(scriptPath)
	if err != nil {
		return err
	}
	prog, err := gparser.ParseFile(nil, fileName, srcBytes, 2)
	if err != nil {
		return err
	}
	newVM := NewGenesisVM(fileName, absPath, c.OS, c.Arch, srcBytes, prog)
	newVM.Compiler = c
	c.vms = append(c.vms, newVM)
	return nil
}

// Do runs all compiler functions once scripts are added to it
func (c *Compiler) Do() error {
	err := c.CreateBuildDir()
	if err != nil {
		return err
	}
	err = c.ProcessMacros()
	if err != nil {
		return err
	}
	err = c.InitializeImports()
	if err != nil {
		return err
	}
	err = c.DetectVersions()
	if err != nil {
		return err
	}
	err = c.GatherAssets()
	if err != nil {
		return err
	}
	err = c.WalkGenesisASTs()
	if err != nil {
		return err
	}
	err = c.LocateGoDependencies()
	if err != nil {
		return err
	}
	err = c.BuildGolangASTs()
	if err != nil {
		return err
	}
	err = c.SwizzleNativeCalls()
	if err != nil {
		return err
	}
	err = c.SanityCheckSwizzles()
	if err != nil {
		return err
	}
	err = c.WritePreloads()
	if err != nil {
		return err
	}
	err = c.WriteScripts()
	if err != nil {
		return err
	}
	err = c.EncodeAssets()
	if err != nil {
		return err
	}
	err = c.WriteVMBundles()
	if err != nil {
		return err
	}
	err = c.CreateEntryPoint()
	if err != nil {
		return err
	}
	err = c.PerformPreCompileObfuscation()
	if err != nil {
		return err
	}
	err = c.BuildNativeBinary()
	if err != nil {
		return err
	}
	return nil
}

// CreateBuildDir creates the compiler's build directory, with an additional asset directory as well
func (c *Compiler) CreateBuildDir() error {
	err := os.MkdirAll(c.BuildDir, 0744)
	if err != nil {
		return fmt.Errorf("cannot create build directory: %v", err)
	}
	err = os.MkdirAll(c.AssetDir(), 0744)
	if err != nil {
		return fmt.Errorf("cannot create asset directory: %v", err)
	}
	return nil
}

// ProcessMacros enumerates the compilers virtual machines with the pre-processor to extract
// compiler macros for each virtual machine
func (c *Compiler) ProcessMacros() error {
	if len(c.vms) == 0 {
		return errNoVM
	}
	var wg sync.WaitGroup
	for _, vm := range c.vms {
		wg.Add(1)
		go func(vm *GenesisVM) {
			vm.ProcessMacros()
			wg.Done()
		}(vm)
	}
	wg.Wait()
	return nil
}

// DetectVersions enumerates all VMs to determine the engine version based on the entrypoint.
// For more information on this, look at GenesisVM.DetectTargetEngineVersion()
func (c *Compiler) DetectVersions() error {
	fns := []func() error{}
	for _, vm := range c.vms {
		fns = append(fns, vm.DetectTargetEngineVersion)
	}
	return computil.ExecuteFuncsInParallel(fns)
}

// GatherAssets enumerates all bundled virtual machines for any embedded assets and copies them
// into the build directory's asset cache
func (c *Compiler) GatherAssets() error {
	fns := []func() error{}
	for _, vm := range c.vms {
		fns = append(fns, vm.CacheAssets)
	}
	return computil.ExecuteFuncsInParallel(fns)
}

// WriteScripts enumerates the compiler's genesis VMs and writes a cached version of the
// genesis source to the asset directory to prevent race condiditons with script filesystem locations
func (c *Compiler) WriteScripts() error {
	fns := []func() error{}
	for _, vm := range c.vms {
		fns = append(fns, vm.WriteScript)
	}
	return computil.ExecuteFuncsInParallel(fns)
}

// InitializeImports enumerates the compiler's genesis VMs and writes a cached version of the
// genesis source to the asset directory to prevent race condiditons with script filesystem locations
func (c *Compiler) InitializeImports() error {
	fns := []func() error{}
	for _, vm := range c.vms {
		fns = append(fns, vm.InitializeGoImports)
	}
	return computil.ExecuteFuncsInParallel(fns)
}

// WalkGenesisASTs scans all genesis VMs scripts to identify Golang packages that have been
// called from inside the script using the namespace identifier from the compiler macros
func (c *Compiler) WalkGenesisASTs() error {
	fns := []func() error{}
	for _, vm := range c.vms {
		fns = append(fns, vm.WalkGenesisAST)
	}
	return computil.ExecuteFuncsInParallel(fns)
}

// LocateGoDependencies gathers a list of all installed golang packages, hands a copy to each VM,
// then has every VM resolve it's own golang dependencies from that package list
func (c *Compiler) LocateGoDependencies() error {
	// grab a list of currently installed golang packages
	gopks, err := gopkgs.Packages(gopkgs.Options{NoVendor: true})
	if err != nil {
		return err
	}

	// enumerate the packages, identifying all VMs that use them
	for _, gopkg := range gopks {
		for _, vm := range c.vms {
			if gop, ok := vm.GoPackageByImport[gopkg.ImportPath]; ok {
				gop.Dir = gopkg.Dir
				gop.ImportPath = gopkg.ImportPath
				gop.Name = gopkg.Name
			}
		}
	}

	// now to check for any unmet dependencies
	packages := map[string]bool{}
	for _, vm := range c.vms {
		for _, p := range vm.UnresolvedGoPackages() {
			packages[p] = true
		}
	}

	// handle the error
	if len(packages) > 0 {
		c.Logger.Errorf("a number of golang dependencies could not be resolved:")
		for k := range packages {
			c.Logger.Errorf("\t%s", k)
		}
		return fmt.Errorf("unresolved golang packages discovered")
	}
	return nil
}

// BuildGolangASTs enumerates each genesis vm's golang native packages and matches exported
// function declarations to their genesis script caller. This creates a reference in the VM's
// linker object which will be used to generate native interfaces between the genesis VM and
// the underlying golang packages.
func (c *Compiler) BuildGolangASTs() error {
	fns := []func() error{}
	for _, vm := range c.vms {
		fns = append(fns, vm.BuildGolangAST)
	}
	return computil.ExecuteFuncsInParallel(fns)
}

// SwizzleNativeCalls enumerates all native golang function calls mapped to genesis script
// function calls and generates the type declarations for both arguments and return values.
func (c *Compiler) SwizzleNativeCalls() error {
	fns := []func() error{}
	for _, vm := range c.vms {
		fns = append(fns, vm.SwizzleNativeFunctionCalls)
	}
	return computil.ExecuteFuncsInParallel(fns)
}

// SanityCheckSwizzles enumerates all VMs to make sure their linked native functions
// are being called correctly by the corrasponding javascript callers
func (c *Compiler) SanityCheckSwizzles() error {
	fns := []func() error{}
	for _, vm := range c.vms {
		fns = append(fns, vm.SanityCheckLinkedSymbols)
	}
	return computil.ExecuteFuncsInParallel(fns)
}

// WritePreloads renders preload libraries for every virtual machine in the compilers asset directory
func (c *Compiler) WritePreloads() error {
	fns := []func() error{}
	for _, vm := range c.vms {
		fns = append(fns, vm.WritePreload)
	}
	return computil.ExecuteFuncsInParallel(fns)
}

// EncodeAssets renders all embedded assets into intermediate representation
func (c *Compiler) EncodeAssets() error {
	fns := []func() error{}
	for _, vm := range c.vms {
		fns = append(fns, vm.EncodeBundledAssets)
	}
	return computil.ExecuteFuncsInParallel(fns)
}

// WriteVMBundles writes the intermediate representation for each virtual machine to it's
// vm bundle file within the build directory
func (c *Compiler) WriteVMBundles() error {
	fns := []func() error{}
	for _, vm := range c.vms {
		fns = append(fns, vm.WriteVMBundle)
	}
	return computil.ExecuteFuncsInParallel(fns)
}

// CreateEntryPoint renders the final main() entry point for the final binary in the build directory
func (c *Compiler) CreateEntryPoint() error {
	c.MapVMsByPriority()
	t, err := computil.Asset("entrypoint.go.tmpl")
	if err != nil {
		return err
	}
	filename := "main.go"
	fileLocation := filepath.Join(c.BuildDir, filename)
	tmpl := template.New(filename)
	tmpl2, err := tmpl.Parse(string(t))
	if err != nil {
		return err
	}
	buf := new(bytes.Buffer)
	err = tmpl2.Execute(buf, c)
	if err != nil {
		return err
	}
	retOpts := imports.Options{
		Comments:  true,
		AllErrors: true,
		TabIndent: false,
		TabWidth:  2,
	}
	newData, err := imports.Process("main.go", buf.Bytes(), &retOpts)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(fileLocation, newData, 0644)
	if err != nil {
		return err
	}
	return nil
}

// PerformPreCompileObfuscation runs the pre-compilation obfuscation routines on the intermediate representation
func (c *Compiler) PerformPreCompileObfuscation() error {
	stylist := obfuscator.NewStylist(c.BuildDir)
	err := stylist.LollerSkateDaStringz()
	if err != nil {
		return err
	}
	err = stylist.AddPurpleHairDyeToRoots()
	if err != nil {
		return err
	}
	err = stylist.GetTheQueenToHerThrown()
	if err != nil {
		return err
	}
	return nil
}

// BuildNativeBinary uses the golang compiler to attempt to build a native binary for
// the target platform specified in the compiler options
func (c *Compiler) BuildNativeBinary() error {
	os.Chdir(c.BuildDir)
	cmd := exec.Command("go", "build", `-ldflags`, `-s -w`, "-o", c.OutputFile)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, fmt.Sprintf("GOOS=%s", c.OS))
	cmd.Env = append(cmd.Env, fmt.Sprintf("GOARCH=%s", c.Arch))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return err
	}
	return nil
}

// MapVMsByPriority creates a pointer mapping of each VM by it's unique priority
func (c *Compiler) MapVMsByPriority() error {
	for _, vm := range c.vms {
		if c.SortedVMs[vm.Priority()] == nil {
			c.SortedVMs[vm.Priority()] = []*GenesisVM{}
			c.UniqPriorities = append(c.UniqPriorities, vm.Priority())
		}
		c.SortedVMs[vm.Priority()] = append(c.SortedVMs[vm.Priority()], vm)
	}
	return nil
}
