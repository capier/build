/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package generators

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/gengo/args"
	"k8s.io/gengo/examples/set-gen/sets"
	"k8s.io/gengo/generator"
	"k8s.io/gengo/namer"
	"k8s.io/gengo/types"

	"github.com/golang/glog"
)

// CustomArgs is used tby the go2idl framework to pass args specific to this
// generator.
type CustomArgs struct {
	BoundingDirs []string // Only deal with types rooted under these dirs.
}

// This is the comment tag that carries parameters for deep-copy generation.
const (
	tagName                     = "k8s:deepcopy-gen"
	interfacesTagName           = tagName + ":interfaces"
	interfacesNonPointerTagName = tagName + ":nonpointer-interfaces" // attach the DeepCopy<Interface> methods to the
)

// Known values for the comment tag.
const tagValuePackage = "package"

// tagValue holds parameters from a tagName tag.
type tagValue struct {
	value    string
	register bool
}

func extractTag(comments []string) *tagValue {
	tagVals := types.ExtractCommentTags("+", comments)[tagName]
	if tagVals == nil {
		// No match for the tag.
		return nil
	}
	// If there are multiple values, abort.
	if len(tagVals) > 1 {
		glog.Fatalf("Found %d %s tags: %q", len(tagVals), tagName, tagVals)
	}

	// If we got here we are returning something.
	tag := &tagValue{}

	// Get the primary value.
	parts := strings.Split(tagVals[0], ",")
	if len(parts) >= 1 {
		tag.value = parts[0]
	}

	// Parse extra arguments.
	parts = parts[1:]
	for i := range parts {
		kv := strings.SplitN(parts[i], "=", 2)
		k := kv[0]
		v := ""
		if len(kv) == 2 {
			v = kv[1]
		}
		switch k {
		case "register":
			if v != "false" {
				tag.register = true
			}
		default:
			glog.Fatalf("Unsupported %s param: %q", tagName, parts[i])
		}
	}
	return tag
}

// TODO: This is created only to reduce number of changes in a single PR.
// Remove it and use PublicNamer instead.
func deepCopyNamer() *namer.NameStrategy {
	return &namer.NameStrategy{
		Join: func(pre string, in []string, post string) string {
			return strings.Join(in, "_")
		},
		PrependPackageNames: 1,
	}
}

// NameSystems returns the name system used by the generators in this package.
func NameSystems() namer.NameSystems {
	return namer.NameSystems{
		"public": deepCopyNamer(),
		"raw":    namer.NewRawNamer("", nil),
	}
}

// DefaultNameSystem returns the default name system for ordering the types to be
// processed by the generators in this package.
func DefaultNameSystem() string {
	return "public"
}

func Packages(context *generator.Context, arguments *args.GeneratorArgs) generator.Packages {
	boilerplate, err := arguments.LoadGoBoilerplate()
	if err != nil {
		glog.Fatalf("Failed loading boilerplate: %v", err)
	}

	inputs := sets.NewString(context.Inputs...)
	packages := generator.Packages{}
	header := append([]byte(fmt.Sprintf("// +build !%s\n\n", arguments.GeneratedBuildTag)), boilerplate...)
	header = append(header, []byte(`
	    // This file was autogenerated by deepcopy-gen. Do not edit it manually!

		`)...)

	boundingDirs := []string{}
	if customArgs, ok := arguments.CustomArgs.(*CustomArgs); ok {
		if customArgs.BoundingDirs == nil {
			customArgs.BoundingDirs = context.Inputs
		}
		for i := range customArgs.BoundingDirs {
			// Strip any trailing slashes - they are not exactly "correct" but
			// this is friendlier.
			boundingDirs = append(boundingDirs, strings.TrimRight(customArgs.BoundingDirs[i], "/"))
		}
	}

	for i := range inputs {
		glog.V(5).Infof("Considering pkg %q", i)
		pkg := context.Universe[i]
		if pkg == nil {
			// If the input had no Go files, for example.
			continue
		}

		ptag := extractTag(pkg.Comments)
		ptagValue := ""
		ptagRegister := false
		if ptag != nil {
			ptagValue = ptag.value
			if ptagValue != tagValuePackage {
				glog.Fatalf("Package %v: unsupported %s value: %q", i, tagName, ptagValue)
			}
			ptagRegister = ptag.register
			glog.V(5).Infof("  tag.value: %q, tag.register: %t", ptagValue, ptagRegister)
		} else {
			glog.V(5).Infof("  no tag")
		}

		// If the pkg-scoped tag says to generate, we can skip scanning types.
		pkgNeedsGeneration := (ptagValue == tagValuePackage)
		if !pkgNeedsGeneration {
			// If the pkg-scoped tag did not exist, scan all types for one that
			// explicitly wants generation.
			for _, t := range pkg.Types {
				glog.V(5).Infof("  considering type %q", t.Name.String())
				ttag := extractTag(t.CommentLines)
				if ttag != nil && ttag.value == "true" {
					glog.V(5).Infof("    tag=true")
					if !copyableType(t) {
						glog.Fatalf("Type %v requests deepcopy generation but is not copyable", t)
					}
					pkgNeedsGeneration = true
					break
				}
			}
		}

		if pkgNeedsGeneration {
			glog.V(3).Infof("Package %q needs generation", i)
			path := pkg.Path
			// if the source path is within a /vendor/ directory (for example,
			// k8s.io/kubernetes/vendor/k8s.io/apimachinery/pkg/apis/meta/v1), allow
			// generation to output to the proper relative path (under vendor).
			// Otherwise, the generator will create the file in the wrong location
			// in the output directory.
			// TODO: build a more fundamental concept in gengo for dealing with modifications
			// to vendored packages.
			if strings.HasPrefix(pkg.SourcePath, arguments.OutputBase) {
				expandedPath := strings.TrimPrefix(pkg.SourcePath, arguments.OutputBase)
				if strings.Contains(expandedPath, "/vendor/") {
					path = expandedPath
				}
			}
			packages = append(packages,
				&generator.DefaultPackage{
					PackageName: strings.Split(filepath.Base(pkg.Path), ".")[0],
					PackagePath: path,
					HeaderText:  header,
					GeneratorFunc: func(c *generator.Context) (generators []generator.Generator) {
						return []generator.Generator{
							NewGenDeepCopy(arguments.OutputFileBaseName, pkg.Path, boundingDirs, (ptagValue == tagValuePackage), ptagRegister),
						}
					},
					FilterFunc: func(c *generator.Context, t *types.Type) bool {
						return t.Name.Package == pkg.Path
					},
				})
		}
	}
	return packages
}

// genDeepCopy produces a file with autogenerated deep-copy functions.
type genDeepCopy struct {
	generator.DefaultGen
	targetPackage string
	boundingDirs  []string
	allTypes      bool
	registerTypes bool
	imports       namer.ImportTracker
	typesForInit  []*types.Type
}

func NewGenDeepCopy(sanitizedName, targetPackage string, boundingDirs []string, allTypes, registerTypes bool) generator.Generator {
	return &genDeepCopy{
		DefaultGen: generator.DefaultGen{
			OptionalName: sanitizedName,
		},
		targetPackage: targetPackage,
		boundingDirs:  boundingDirs,
		allTypes:      allTypes,
		registerTypes: registerTypes,
		imports:       generator.NewImportTracker(),
		typesForInit:  make([]*types.Type, 0),
	}
}

func (g *genDeepCopy) Namers(c *generator.Context) namer.NameSystems {
	// Have the raw namer for this file track what it imports.
	return namer.NameSystems{
		"raw": namer.NewRawNamer(g.targetPackage, g.imports),
	}
}

func (g *genDeepCopy) Filter(c *generator.Context, t *types.Type) bool {
	// Filter out types not being processed or not copyable within the package.
	enabled := g.allTypes
	if !enabled {
		ttag := extractTag(t.CommentLines)
		if ttag != nil && ttag.value == "true" {
			enabled = true
		}
	}
	if !enabled {
		return false
	}
	if !copyableType(t) {
		glog.V(2).Infof("Type %v is not copyable", t)
		return false
	}
	glog.V(4).Infof("Type %v is copyable", t)
	g.typesForInit = append(g.typesForInit, t)
	return true
}

func (g *genDeepCopy) copyableAndInBounds(t *types.Type) bool {
	if !copyableType(t) {
		return false
	}
	// Only packages within the restricted range can be processed.
	if !isRootedUnder(t.Name.Package, g.boundingDirs) {
		return false
	}
	return true
}

// hasDeepCopyMethod returns true if an appropriate DeepCopy() method is
// defined for the given type.  This allows more efficient deep copy
// implementations to be defined by the type's author.  The correct signature
// for a type T is:
//    func (t T) DeepCopy() T
// or:
//    func (t *T) DeepCopy() T
func hasDeepCopyMethod(t *types.Type) bool {
	for mn, mt := range t.Methods {
		if mn != "DeepCopy" {
			continue
		}
		if len(mt.Signature.Parameters) != 0 {
			return false
		}
		if len(mt.Signature.Results) != 1 || mt.Signature.Results[0].Name != t.Name {
			return false
		}
		return true
	}
	return false
}

func isRootedUnder(pkg string, roots []string) bool {
	// Add trailing / to avoid false matches, e.g. foo/bar vs foo/barn.  This
	// assumes that bounding dirs do not have trailing slashes.
	pkg = pkg + "/"
	for _, root := range roots {
		if strings.HasPrefix(pkg, root+"/") {
			return true
		}
	}
	return false
}

func copyableType(t *types.Type) bool {
	// If the type opts out of copy-generation, stop.
	ttag := extractTag(t.CommentLines)
	if ttag != nil && ttag.value == "false" {
		return false
	}
	// TODO: Consider generating functions for other kinds too.
	if t.Kind != types.Struct {
		return false
	}
	// Also, filter out private types.
	if namer.IsPrivateGoName(t.Name.Name) {
		return false
	}
	return true
}

func (g *genDeepCopy) isOtherPackage(pkg string) bool {
	if pkg == g.targetPackage {
		return false
	}
	if strings.HasSuffix(pkg, "\""+g.targetPackage+"\"") {
		return false
	}
	return true
}

func (g *genDeepCopy) Imports(c *generator.Context) (imports []string) {
	importLines := []string{}
	for _, singleImport := range g.imports.ImportLines() {
		if g.isOtherPackage(singleImport) {
			importLines = append(importLines, singleImport)
		}
	}
	return importLines
}

func argsFromType(ts ...*types.Type) generator.Args {
	a := generator.Args{
		"type": ts[0],
	}
	for i, t := range ts {
		a[fmt.Sprintf("type%d", i+1)] = t
	}
	return a
}

func (g *genDeepCopy) Init(c *generator.Context, w io.Writer) error {
	return nil
}

func (g *genDeepCopy) needsGeneration(t *types.Type) bool {
	tag := extractTag(t.CommentLines)
	tv := ""
	if tag != nil {
		tv = tag.value
		if tv != "true" && tv != "false" {
			glog.Fatalf("Type %v: unsupported %s value: %q", t, tagName, tag.value)
		}
	}
	if g.allTypes && tv == "false" {
		// The whole package is being generated, but this type has opted out.
		glog.V(5).Infof("Not generating for type %v because type opted out", t)
		return false
	}
	if !g.allTypes && tv != "true" {
		// The whole package is NOT being generated, and this type has NOT opted in.
		glog.V(5).Infof("Not generating for type %v because type did not opt in", t)
		return false
	}
	return true
}

func extractInterfacesTag(comments []string) []string {
	var result []string
	values := types.ExtractCommentTags("+", comments)[interfacesTagName]
	for _, v := range values {
		if len(v) == 0 {
			continue
		}
		intfs := strings.Split(v, ",")
		for _, intf := range intfs {
			if intf == "" {
				continue
			}
			result = append(result, intf)
		}
	}
	return result
}

func extractNonPointerInterfaces(comments []string) (bool, error) {
	values := types.ExtractCommentTags("+", comments)[interfacesNonPointerTagName]
	if len(values) == 0 {
		return false, nil
	}
	result := values[0] == "true"
	for _, v := range values {
		if v == "true" != result {
			return false, fmt.Errorf("contradicting %v value %q found to previous value %v", interfacesNonPointerTagName, v, result)
		}
	}
	return result, nil
}

func (g *genDeepCopy) deepCopyableInterfaces(c *generator.Context, t *types.Type) ([]*types.Type, error) {
	if t.Kind != types.Struct {
		return nil, nil
	}

	intfs := extractInterfacesTag(append(t.SecondClosestCommentLines, t.CommentLines...))

	var ts []*types.Type
	for _, intf := range intfs {
		t := types.ParseFullyQualifiedName(intf)
		c.AddDir(t.Package)
		intfT := c.Universe.Type(t)
		if intfT == nil {
			return nil, fmt.Errorf("unknown type %q in %s tag of type %s", intf, interfacesTagName, intfT)
		}
		if intfT.Kind != types.Interface {
			return nil, fmt.Errorf("type %q in %s tag of type %s is not an interface, but: %q", intf, interfacesTagName, t, intfT.Kind)
		}
		g.imports.AddType(intfT)
		ts = append(ts, intfT)
	}

	return ts, nil
}

// DeepCopyableInterfaces returns the interface types to implement and whether they apply to a non-pointer receiver.
func (g *genDeepCopy) DeepCopyableInterfaces(c *generator.Context, t *types.Type) ([]*types.Type, bool, error) {
	ts, err := g.deepCopyableInterfaces(c, t)
	if err != nil {
		return nil, false, err
	}

	set := map[string]*types.Type{}
	for _, t := range ts {
		set[t.String()] = t
	}

	result := []*types.Type{}
	for _, t := range set {
		result = append(result, t)
	}

	TypeSlice(result).Sort() // we need a stable sorting because it determines the order in generation

	nonPointerReceiver, err := extractNonPointerInterfaces(append(t.SecondClosestCommentLines, t.CommentLines...))
	if err != nil {
		return nil, false, err
	}

	return result, nonPointerReceiver, nil
}

type TypeSlice []*types.Type

func (s TypeSlice) Len() int           { return len(s) }
func (s TypeSlice) Less(i, j int) bool { return s[i].String() < s[j].String() }
func (s TypeSlice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s TypeSlice) Sort()              { sort.Sort(s) }

func (g *genDeepCopy) GenerateType(c *generator.Context, t *types.Type, w io.Writer) error {
	if !g.needsGeneration(t) {
		return nil
	}
	glog.V(5).Infof("Generating deepcopy function for type %v", t)

	sw := generator.NewSnippetWriter(w, c, "$", "$")
	args := argsFromType(t)

	_, foundDeepCopyInto := t.Methods["DeepCopyInto"]
	_, foundDeepCopy := t.Methods["DeepCopy"]
	if !foundDeepCopyInto {
		sw.Do("// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.\n", args)
		sw.Do("func (in *$.type|raw$) DeepCopyInto(out *$.type|raw$) {\n", args)
		if foundDeepCopy {
			if t.Methods["DeepCopy"].Signature.Receiver.Kind == types.Pointer {
				sw.Do("clone := in.DeepCopy()\n", nil)
				sw.Do("*out = *clone\n", nil)
			} else {
				sw.Do("*out = in.DeepCopy()\n", nil)
			}
			sw.Do("return\n", nil)
		} else {
			g.generateFor(t, sw)
			sw.Do("return\n", nil)
		}
		sw.Do("}\n\n", nil)
	}

	if !foundDeepCopy {
		sw.Do("// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new $.type|raw$.\n", args)
		sw.Do("func (in *$.type|raw$) DeepCopy() *$.type|raw$ {\n", args)
		sw.Do("if in == nil { return nil }\n", nil)
		sw.Do("out := new($.type|raw$)\n", args)
		sw.Do("in.DeepCopyInto(out)\n", nil)
		sw.Do("return out\n", nil)
		sw.Do("}\n\n", nil)
	}

	intfs, nonPointerReceiver, err := g.DeepCopyableInterfaces(c, t)
	if err != nil {
		return err
	}
	for _, intf := range intfs {
		sw.Do(fmt.Sprintf("// DeepCopy%s is an autogenerated deepcopy function, copying the receiver, creating a new $.type2|raw$.\n", intf.Name.Name), argsFromType(t, intf))
		if nonPointerReceiver {
			sw.Do(fmt.Sprintf("func (in $.type|raw$) DeepCopy%s() $.type2|raw$ {\n", intf.Name.Name), argsFromType(t, intf))
			sw.Do("return *in.DeepCopy()", nil)
			sw.Do("}\n\n", nil)
		} else {
			sw.Do(fmt.Sprintf("func (in *$.type|raw$) DeepCopy%s() $.type2|raw$ {\n", intf.Name.Name), argsFromType(t, intf))
			sw.Do("if c := in.DeepCopy(); c != nil {\n", nil)
			sw.Do("return c\n", nil)
			sw.Do("}\n", nil)
			sw.Do("return nil\n", nil)
			sw.Do("}\n\n", nil)
		}
	}

	return sw.Error()
}

// we use the system of shadowing 'in' and 'out' so that the same code is valid
// at any nesting level. This makes the autogenerator easy to understand, and
// the compiler shouldn't care.
func (g *genDeepCopy) generateFor(t *types.Type, sw *generator.SnippetWriter) {
	var f func(*types.Type, *generator.SnippetWriter)
	switch t.Kind {
	case types.Builtin:
		f = g.doBuiltin
	case types.Map:
		f = g.doMap
	case types.Slice:
		f = g.doSlice
	case types.Struct:
		f = g.doStruct
	case types.Interface:
		f = g.doInterface
	case types.Pointer:
		f = g.doPointer
	case types.Alias:
		f = g.doAlias
	default:
		f = g.doUnknown
	}
	f(t, sw)
}

func (g *genDeepCopy) doBuiltin(t *types.Type, sw *generator.SnippetWriter) {
	sw.Do("*out = *in\n", nil)
}

func (g *genDeepCopy) doMap(t *types.Type, sw *generator.SnippetWriter) {
	sw.Do("*out = make($.|raw$, len(*in))\n", t)
	if t.Key.IsAssignable() {
		switch {
		case hasDeepCopyMethod(t.Elem):
			sw.Do("for key, val := range *in {\n", nil)
			sw.Do("(*out)[key] = val.DeepCopy()\n", nil)
			sw.Do("}\n", nil)
		case t.Elem.IsAnonymousStruct():
			sw.Do("for key := range *in {\n", nil)
			sw.Do("(*out)[key] = struct{}{}\n", nil)
			sw.Do("}\n", nil)
		case t.Elem.IsAssignable():
			sw.Do("for key, val := range *in {\n", nil)
			sw.Do("(*out)[key] = val\n", nil)
			sw.Do("}\n", nil)
		case t.Elem.Kind == types.Interface:
			sw.Do("for key, val := range *in {\n", nil)
			sw.Do("if val == nil {(*out)[key]=nil} else {\n", nil)
			sw.Do(fmt.Sprintf("(*out)[key] = val.DeepCopy%s()\n", t.Elem.Name.Name), t)
			sw.Do("}}\n", nil)
		default:
			sw.Do("for key, val := range *in {\n", nil)
			if g.copyableAndInBounds(t.Elem) {
				sw.Do("newVal := new($.|raw$)\n", t.Elem)
				sw.Do("val.DeepCopyInto(newVal)\n", nil)
				sw.Do("(*out)[key] = *newVal\n", nil)
			} else if t.Elem.Kind == types.Slice && t.Elem.Elem.Kind == types.Builtin {
				sw.Do("if val==nil { (*out)[key]=nil } else {\n", nil)
				sw.Do("(*out)[key] = make($.|raw$, len(val))\n", t.Elem)
				sw.Do("copy((*out)[key], val)\n", nil)
				sw.Do("}\n", nil)
			} else if t.Elem.Kind == types.Alias && t.Elem.Underlying.Kind == types.Slice && t.Elem.Underlying.Elem.Kind == types.Builtin {
				sw.Do("(*out)[key] = make($.|raw$, len(val))\n", t.Elem)
				sw.Do("copy((*out)[key], val)\n", nil)
			} else if t.Elem.Kind == types.Pointer {
				sw.Do("if val==nil { (*out)[key]=nil } else {\n", nil)
				sw.Do("(*out)[key] = new($.Elem|raw$)\n", t.Elem)
				sw.Do("val.DeepCopyInto((*out)[key])\n", nil)
				sw.Do("}\n", nil)
			} else {
				sw.Do("(*out)[key] = *val.DeepCopy()\n", t.Elem)
			}
			sw.Do("}\n", nil)
		}
	} else {
		// TODO: Implement it when necessary.
		sw.Do("for range *in {\n", nil)
		sw.Do("// FIXME: Copying unassignable keys unsupported $.|raw$\n", t.Key)
		sw.Do("}\n", nil)
	}
}

func (g *genDeepCopy) doSlice(t *types.Type, sw *generator.SnippetWriter) {
	if hasDeepCopyMethod(t) {
		sw.Do("*out = in.DeepCopy()\n", nil)
		return
	}

	sw.Do("*out = make($.|raw$, len(*in))\n", t)
	if hasDeepCopyMethod(t.Elem) {
		sw.Do("for i := range *in {\n", nil)
		sw.Do("(*out)[i] = (*in)[i].DeepCopy()\n", nil)
		sw.Do("}\n", nil)
	} else if t.Elem.Kind == types.Builtin || t.Elem.IsAssignable() {
		sw.Do("copy(*out, *in)\n", nil)
	} else {
		sw.Do("for i := range *in {\n", nil)
		if t.Elem.Kind == types.Slice {
			sw.Do("if (*in)[i] != nil {\n", nil)
			sw.Do("in, out := &(*in)[i], &(*out)[i]\n", nil)
			g.generateFor(t.Elem, sw)
			sw.Do("}\n", nil)
		} else if hasDeepCopyMethod(t.Elem) {
			sw.Do("(*out)[i] = (*in)[i].DeepCopy()\n", nil)
			// REVISIT(sttts): the following is removed in master
			//} else if t.Elem.IsAssignable() {
			//	sw.Do("(*out)[i] = (*in)[i]\n", nil)
		} else if t.Elem.Kind == types.Interface {
			sw.Do("if (*in)[i] == nil {(*out)[i]=nil} else {\n", nil)
			sw.Do(fmt.Sprintf("(*out)[i] = (*in)[i].DeepCopy%s()\n", t.Elem.Name.Name), t)
			sw.Do("}\n", nil)
		} else if t.Elem.Kind == types.Pointer {
			sw.Do("if (*in)[i]==nil { (*out)[i]=nil } else {\n", nil)
			sw.Do("(*out)[i] = new($.Elem|raw$)\n", t.Elem)
			sw.Do("(*in)[i].DeepCopyInto((*out)[i])\n", nil)
			sw.Do("}\n", nil)
		} else if t.Elem.Kind == types.Struct {
			sw.Do("(*in)[i].DeepCopyInto(&(*out)[i])\n", nil)
		} else {
			sw.Do("(*out)[i] = (*in)[i].DeepCopy()\n", nil)
		}
		sw.Do("}\n", nil)
	}
}

func (g *genDeepCopy) doStruct(t *types.Type, sw *generator.SnippetWriter) {
	if hasDeepCopyMethod(t) {
		sw.Do("*out = in.DeepCopy()\n", nil)
		return
	}

	// Simple copy covers a lot of cases.
	sw.Do("*out = *in\n", nil)

	// Now fix-up fields as needed.
	for _, m := range t.Members {
		t := m.Type
		hasMethod := hasDeepCopyMethod(t)
		if t.Kind == types.Alias {
			copied := *t.Underlying
			copied.Name = t.Name
			t = &copied
		}
		args := generator.Args{
			"type": t,
			"kind": t.Kind,
			"name": m.Name,
		}
		switch t.Kind {
		case types.Builtin:
			if hasMethod {
				sw.Do("out.$.name$ = in.$.name$.DeepCopy()\n", args)
			}
			// the initial *out = *in was enough
		case types.Map, types.Slice, types.Pointer:
			if hasMethod {
				sw.Do("if in.$.name$ != nil {\n", args)
				sw.Do("out.$.name$ = in.$.name$.DeepCopy()\n", args)
				sw.Do("}\n", nil)
			} else {
				// Fixup non-nil reference-semantic types.
				sw.Do("if in.$.name$ != nil {\n", args)
				sw.Do("in, out := &in.$.name$, &out.$.name$\n", args)
				g.generateFor(t, sw)
				sw.Do("}\n", nil)
			}
		case types.Struct:
			if hasMethod {
				sw.Do("out.$.name$ = in.$.name$.DeepCopy()\n", args)
			} else if t.IsAssignable() {
				sw.Do("out.$.name$ = in.$.name$\n", args)
			} else {
				sw.Do("in.$.name$.DeepCopyInto(&out.$.name$)\n", args)
			}
		case types.Interface:
			sw.Do("if in.$.name$ == nil {out.$.name$=nil} else {\n", args)
			sw.Do(fmt.Sprintf("out.$.name$ = in.$.name$.DeepCopy%s()\n", t.Name.Name), args)
			sw.Do("}\n", nil)
		default:
			sw.Do("out.$.name$ = in.$.name$.DeepCopy()\n", args)
		}
	}
}

func (g *genDeepCopy) doInterface(t *types.Type, sw *generator.SnippetWriter) {
	// TODO: Add support for interfaces.
	g.doUnknown(t, sw)
}

func (g *genDeepCopy) doPointer(t *types.Type, sw *generator.SnippetWriter) {
	sw.Do("if *in == nil { *out = nil } else {\n", t)
	if hasDeepCopyMethod(t.Elem) {
		sw.Do("*out = new($.Elem|raw$)\n", t)
		sw.Do("**out = (*in).DeepCopy()\n", nil)
	} else if t.Elem.IsAssignable() {
		sw.Do("*out = new($.Elem|raw$)\n", t)
		sw.Do("**out = **in", nil)
	} else {
		switch t.Elem.Kind {
		case types.Map, types.Slice:
			sw.Do("*out = new($.Elem|raw$)\n", t)
			sw.Do("if **in != nil {\n", t)
			sw.Do("in, out := *in, *out\n", nil)
			g.generateFor(t.Elem, sw)
			sw.Do("}\n", nil)
		default:
			sw.Do("*out = new($.Elem|raw$)\n", t)
			sw.Do("(*in).DeepCopyInto(*out)\n", nil)
		}
	}
	sw.Do("}", t)
}

func (g *genDeepCopy) doAlias(t *types.Type, sw *generator.SnippetWriter) {
	// TODO: Add support for aliases.
	g.doUnknown(t, sw)
}

func (g *genDeepCopy) doUnknown(t *types.Type, sw *generator.SnippetWriter) {
	sw.Do("// FIXME: Type $.|raw$ is unsupported.\n", t)
}
