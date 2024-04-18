// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package configs

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	version "github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2"

	"github.com/hashicorp/terraform/internal/addrs"
)

// BuildConfig constructs a Config from a root module by loading all of its
// descendent modules via the given ModuleWalker. This function also side loads
// and installs any mock data files needed by the testing framework via the
// MockDataLoader.
//
// The result is a module tree that has so far only had basic module- and
// file-level invariants validated. If the returned diagnostics contains errors,
// the returned module tree may be incomplete but can still be used carefully
// for static analysis.
func BuildConfig(ctx context.Context, root *Module, walker ModuleWalker, loader MockDataLoader) (*Config, hcl.Diagnostics, []*ModuleDeprecationInfo) {
	ctx, span := tracer.Start(ctx, "build config")
	defer span.End()
	var diags hcl.Diagnostics
	var modDeprecations []*ModuleDeprecationInfo
	cfg := &Config{
		Module: root,
	}
	cfg.Root = cfg // Root module is self-referential.
	cfg.Children, diags, modDeprecations = buildChildModules(ctx, cfg, walker)
	diags = append(diags, buildTestModules(ctx, cfg, walker)...)

	// Skip provider resolution if there are any errors, since the provider
	// configurations themselves may not be valid.
	if !diags.HasErrors() {
		// Now that the config is built, we can connect the provider names to all
		// the known types for validation.
		providers := cfg.resolveProviderTypes()
		cfg.resolveProviderTypesForTests(providers)
	}

	diags = append(diags, validateProviderConfigs(nil, cfg, nil)...)
	diags = append(diags, validateProviderConfigsForTests(cfg)...)

	// Final step, let's side load any external mock data into our test files.
	diags = append(diags, installMockDataFiles(cfg, loader)...)

	return cfg, diags, modDeprecations
}

func installMockDataFiles(root *Config, loader MockDataLoader) hcl.Diagnostics {
	var diags hcl.Diagnostics

	for _, file := range root.Module.Tests {
		for _, provider := range file.Providers {
			if !provider.Mock {
				// Don't try and process non-mocked providers.
				continue
			}

			data, dataDiags := loader.LoadMockData(provider)
			diags = append(diags, dataDiags...)
			if data != nil {
				// If we loaded some data, then merge the new data into the old
				// data. In this case we expect and accept collisions, so we
				// don't want the merge function warning us about them.
				diags = append(diags, provider.MockData.Merge(data, true)...)
			}
		}
	}

	return diags
}

func buildTestModules(ctx context.Context, root *Config, walker ModuleWalker) hcl.Diagnostics {
	var diags hcl.Diagnostics

	for name, file := range root.Module.Tests {
		for _, run := range file.Runs {
			if run.Module == nil {
				continue
			}

			// We want to make sure the path for the testing modules are unique
			// so we create a dedicated path for them.
			//
			// Some examples:
			//    - file: main.tftest.hcl, run: setup - test.main.setup
			//    - file: tests/main.tftest.hcl, run: setup - test.tests.main.setup

			dir := path.Dir(name)
			base := path.Base(name)

			path := addrs.Module{}
			path = append(path, "test")
			if dir != "." {
				path = append(path, strings.Split(dir, "/")...)
			}
			path = append(path, strings.TrimSuffix(base, ".tftest.hcl"), run.Name)

			req := ModuleRequest{
				Name:              run.Name,
				Path:              path,
				SourceAddr:        run.Module.Source,
				SourceAddrRange:   run.Module.SourceDeclRange,
				VersionConstraint: run.Module.Version,
				Parent:            root,
				CallRange:         run.Module.DeclRange,
			}

			// mdTODO: don't think mod deprecations are relevant here, check!
			cfg, modDiags, _ := loadModule(ctx, root, &req, walker)
			diags = append(diags, modDiags...)

			if cfg != nil {
				// To get the loader to work, we need to set a bunch of values
				// (like the name, path, and parent) as if the module was being
				// loaded as a child of the root config.
				//
				// In actuality, when this is executed it will be as if the
				// module was the root. So, we'll post-process some things to
				// get it to behave as expected later.

				// First, update the main module for this test run to behave as
				// if it is the root module.
				cfg.Parent = nil

				// Then we need to update the paths for this config and all
				// children, so they think they are all relative to the root
				// module we just created.
				rebaseChildModule(cfg, cfg)

				// Finally, link the new config back into our test run so
				// it can be retrieved later.
				run.ConfigUnderTest = cfg
			}
		}
	}

	return diags
}

func buildChildModules(ctx context.Context, parent *Config, walker ModuleWalker) (map[string]*Config, hcl.Diagnostics, []*ModuleDeprecationInfo) {
	ctx, span := tracer.Start(ctx, "build child modules")
	defer span.End()
	var diags hcl.Diagnostics
	modDeprecations := []*ModuleDeprecationInfo{}
	ret := map[string]*Config{}

	calls := parent.Module.ModuleCalls

	// We'll sort the calls by their local names so that they'll appear in a
	// predictable order in any logging that's produced during the walk.
	callNames := make([]string, 0, len(calls))
	for k := range calls {
		callNames = append(callNames, k)
	}
	sort.Strings(callNames)

	for _, callName := range callNames {
		call := calls[callName]
		path := make([]string, len(parent.Path)+1)
		copy(path, parent.Path)
		path[len(path)-1] = call.Name

		req := ModuleRequest{
			Name:              call.Name,
			Path:              path,
			SourceAddr:        call.SourceAddr,
			SourceAddrRange:   call.SourceAddrRange,
			VersionConstraint: call.Version,
			Parent:            parent,
			CallRange:         call.DeclRange,
		}
		child, modDiags, modDeprecation := loadModule(ctx, parent.Root, &req, walker)
		diags = append(diags, modDiags...)
		modDeprecations = append(modDeprecations, modDeprecation)
		if child == nil {
			// This means an error occurred, there should be diagnostics within
			// modDiags for this.
			continue
		}

		ret[call.Name] = child
	}

	return ret, diags, modDeprecations
}

func loadModule(ctx context.Context, root *Config, req *ModuleRequest, walker ModuleWalker) (*Config, hcl.Diagnostics, *ModuleDeprecationInfo) {
	var diags hcl.Diagnostics
	var modDeprecation *ModuleDeprecationInfo
	var childModDeprecations []*ModuleDeprecationInfo

	mod, ver, modDiags, modDeprecation := walker.LoadModule(ctx, req)
	diags = append(diags, modDiags...)
	if mod == nil {
		// nil can be returned if the source address was invalid and so
		// nothing could be loaded whatsoever. LoadModule should've
		// returned at least one error diagnostic in that case.
		return nil, diags, nil
	}

	cfg := &Config{
		Parent:          req.Parent,
		Root:            root,
		Path:            req.Path,
		Module:          mod,
		CallRange:       req.CallRange,
		SourceAddr:      req.SourceAddr,
		SourceAddrRange: req.SourceAddrRange,
		Version:         ver,
	}

	cfg.Children, modDiags, childModDeprecations = buildChildModules(ctx, cfg, walker)
	diags = append(diags, modDiags...)
	// mdTODO: Should we return something from the non registry module install methods? Might be more sensable there rather than here.
	// if the module is not a registry module we won't have a parent ModuleDeprecationInfo to attach any registry modules it has as external dependencies
	if _, isRegistryModule := req.SourceAddr.(addrs.ModuleSourceRegistry); !isRegistryModule {
		modDeprecation = &ModuleDeprecationInfo{
			SourceName:           req.Name,
			RegistryDeprecation:  nil,
			ExternalDependencies: []*ModuleDeprecationInfo{},
		}
	}
	// mdTODO: better error handling here, think of some more ways this can break
	if modDeprecation != nil && childModDeprecations != nil {
		modDeprecation.ExternalDependencies = childModDeprecations
	}
	if mod.Backend != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  "Backend configuration ignored",
			Detail:   "Any selected backend applies to the entire configuration, so Terraform expects provider configurations only in the root module.\n\nThis is a warning rather than an error because it's sometimes convenient to temporarily call a root module as a child module for testing purposes, but this backend configuration block will have no effect.",
			Subject:  mod.Backend.DeclRange.Ptr(),
		})
	}

	if len(mod.Import) > 0 {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid import configuration",
			Detail:   fmt.Sprintf("An import block was detected in %q. Import blocks are only allowed in the root module.", cfg.Path),
			Subject:  mod.Import[0].DeclRange.Ptr(),
		})
	}

	return cfg, diags, modDeprecation
}

// rebaseChildModule updates cfg to make it act as if root is the base of the
// module tree.
//
// This is used for modules loaded directly from test files. In order to load
// them properly, and reuse the code for loading modules from normal
// configuration files, we pretend they are children of the main configuration
// object. Later, when it comes time for them to execute they will act as if
// they are the root module directly.
//
// This function updates cfg so that it treats the provided root as the actual
// root of this module tree. It then recurses into all the child modules and
// does the same for them.
func rebaseChildModule(cfg *Config, root *Config) {
	for _, child := range cfg.Children {
		rebaseChildModule(child, root)
	}

	cfg.Path = cfg.Path[len(root.Path):]
	cfg.Root = root
}

type ModuleDeprecationInfo struct {
	SourceName           string
	RegistryDeprecation  *RegistryModuleDeprecation
	ExternalDependencies []*ModuleDeprecationInfo
}

type RegistryModuleDeprecation struct {
	Subject      *hcl.Range
	Message      string
	ExternalLink string
}

// A ModuleWalker knows how to find and load a child module given details about
// the module to be loaded and a reference to its partially-loaded parent
// Config.
type ModuleWalker interface {
	// LoadModule finds and loads a requested child module.
	//
	// If errors are detected during loading, implementations should return them
	// in the diagnostics object. If the diagnostics object contains any errors
	// then the caller will tolerate the returned module being nil or incomplete.
	// If no errors are returned, it should be non-nil and complete.
	//
	// Full validation need not have been performed but an implementation should
	// ensure that the basic file- and module-validations performed by the
	// LoadConfigDir function (valid syntax, no namespace collisions, etc) have
	// been performed before returning a module.
	LoadModule(ctx context.Context, req *ModuleRequest) (*Module, *version.Version, hcl.Diagnostics, *ModuleDeprecationInfo)
}

// ModuleWalkerFunc is an implementation of ModuleWalker that directly wraps
// a callback function, for more convenient use of that interface.
type ModuleWalkerFunc func(ctx context.Context, req *ModuleRequest) (*Module, *version.Version, hcl.Diagnostics, *ModuleDeprecationInfo)

// LoadModule implements ModuleWalker.
func (f ModuleWalkerFunc) LoadModule(ctx context.Context, req *ModuleRequest) (*Module, *version.Version, hcl.Diagnostics, *ModuleDeprecationInfo) {
	return f(ctx, req)
}

// ModuleRequest is used with the ModuleWalker interface to describe a child
// module that must be loaded.
type ModuleRequest struct {
	// Name is the "logical name" of the module call within configuration.
	// This is provided in case the name is used as part of a storage key
	// for the module, but implementations must otherwise treat it as an
	// opaque string. It is guaranteed to have already been validated as an
	// HCL identifier and UTF-8 encoded.
	Name string

	// Path is a list of logical names that traverse from the root module to
	// this module. This can be used, for example, to form a lookup key for
	// each distinct module call in a configuration, allowing for multiple
	// calls with the same name at different points in the tree.
	Path addrs.Module

	// SourceAddr is the source address string provided by the user in
	// configuration.
	SourceAddr addrs.ModuleSource

	// SourceAddrRange is the source range for the SourceAddr value as it
	// was provided in configuration. This can and should be used to generate
	// diagnostics about the source address having invalid syntax, referring
	// to a non-existent object, etc.
	SourceAddrRange hcl.Range

	// VersionConstraint is the version constraint applied to the module in
	// configuration. This data structure includes the source range for
	// the constraint, which can and should be used to generate diagnostics
	// about constraint-related issues, such as constraints that eliminate all
	// available versions of a module whose source is otherwise valid.
	VersionConstraint VersionConstraint

	// Parent is the partially-constructed module tree node that the loaded
	// module will be added to. Callers may refer to any field of this
	// structure except Children, which is still under construction when
	// ModuleRequest objects are created and thus has undefined content.
	// The main reason this is provided is so that full module paths can
	// be constructed for uniqueness.
	Parent *Config

	// CallRange is the source range for the header of the "module" block
	// in configuration that prompted this request. This can be used as the
	// subject of an error diagnostic that relates to the module call itself,
	// rather than to either its source address or its version number.
	CallRange hcl.Range
}

// DisabledModuleWalker is a ModuleWalker that doesn't support
// child modules at all, and so will return an error if asked to load one.
//
// This is provided primarily for testing. There is no good reason to use this
// in the main application.
var DisabledModuleWalker ModuleWalker

func init() {
	DisabledModuleWalker = ModuleWalkerFunc(func(ctx context.Context, req *ModuleRequest) (*Module, *version.Version, hcl.Diagnostics, *ModuleDeprecationInfo) {
		return nil, nil, hcl.Diagnostics{
			{
				Severity: hcl.DiagError,
				Summary:  "Child modules are not supported",
				Detail:   "Child module calls are not allowed in this context.",
				Subject:  &req.CallRange,
			},
		}, nil
	})
}

// MockDataLoader provides an interface similar to loading modules, except it loads
// and returns MockData objects for the testing framework to consume.
type MockDataLoader interface {
	// LoadMockData accepts a path to a local directory that should contain a
	// set of .tfmock.hcl files that contain mock data that can be consumed by
	// a mock provider within the tewting framework.
	LoadMockData(provider *Provider) (*MockData, hcl.Diagnostics)
}

// MockDataLoaderFunc is an implementation of MockDataLoader that wraps a
// callback function, for more convenient use of that interface.
type MockDataLoaderFunc func(provider *Provider) (*MockData, hcl.Diagnostics)

// LoadMockData implements MockDataLoader.
func (f MockDataLoaderFunc) LoadMockData(provider *Provider) (*MockData, hcl.Diagnostics) {
	return f(provider)
}
