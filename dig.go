// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package dig

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/dig/internal/digreflect"
	"go.uber.org/dig/internal/dot"
)

const (
	_optionalTag = "optional"
	_nameTag     = "name"
	_groupTag    = "group"
)

// Unique identification of an object in the graph.
type key struct {
	t reflect.Type

	// Only one of name or group will be set.
	name  string
	group string
}

// Option configures a Container. It's included for future functionality;
// currently, there are no concrete implementations.
type Option interface {
	applyOption(*Container)
}

type optionFunc func(*Container)

func (f optionFunc) applyOption(c *Container) { f(c) }

type provideOptions struct {
	Name  string
	Group string
	As    []interface{}
}

func (o *provideOptions) Validate() error {
	if len(o.Group) > 0 {
		if len(o.Name) > 0 {
			return fmt.Errorf(
				"cannot use named values with value groups: name:%q provided with group:%q", o.Name, o.Group)
		}
		if len(o.As) > 0 {
			return fmt.Errorf(
				"cannot use dig.As with value groups: dig.As provided with group:%q", o.Group)
		}
	}

	// Names must be representable inside a backquoted string. The only
	// limitation for raw string literals as per
	// https://golang.org/ref/spec#raw_string_lit is that they cannot contain
	// backquotes.
	if strings.ContainsRune(o.Name, '`') {
		return fmt.Errorf("invalid dig.Name(%q): names cannot contain backquotes", o.Name)
	}
	if strings.ContainsRune(o.Group, '`') {
		return fmt.Errorf("invalid dig.Group(%q): group names cannot contain backquotes", o.Group)
	}

	for _, i := range o.As {
		t := reflect.TypeOf(i)

		if t == nil {
			return fmt.Errorf("invalid dig.As(nil): argument must be a pointer to an interface")
		}

		if t.Kind() != reflect.Ptr {
			return fmt.Errorf("invalid dig.As(%v): argument must be a pointer to an interface", t)
		}

		pointingTo := t.Elem()
		if pointingTo.Kind() != reflect.Interface {
			return fmt.Errorf("invalid dig.As(*%v): argument must be a pointer to an interface", pointingTo)
		}
	}
	return nil
}

// A ProvideOption modifies the default behavior of Provide.
type ProvideOption interface {
	applyProvideOption(*provideOptions)
}

type provideOptionFunc func(*provideOptions)

func (f provideOptionFunc) applyProvideOption(opts *provideOptions) { f(opts) }

// Name is a ProvideOption that specifies that all values produced by a
// constructor should have the given name. See also the package documentation
// about Named Values.
//
// Given,
//
//   func NewReadOnlyConnection(...) (*Connection, error)
//   func NewReadWriteConnection(...) (*Connection, error)
//
// The following will provide two connections to the container: one under the
// name "ro" and the other under the name "rw".
//
//   c.Provide(NewReadOnlyConnection, dig.Name("ro"))
//   c.Provide(NewReadWriteConnection, dig.Name("rw"))
//
// This option cannot be provided for constructors which produce result
// objects.
func Name(name string) ProvideOption {
	return provideOptionFunc(func(opts *provideOptions) {
		opts.Name = name
	})
}

// Group is a ProvideOption that specifies that all values produced by a
// constructor should be added to the specified group. See also the package
// documentation about Value Groups.
//
// This option cannot be provided for constructors which produce result
// objects.
func Group(group string) ProvideOption {
	return provideOptionFunc(func(opts *provideOptions) {
		opts.Group = group
	})
}

// As is a ProvideOption that specifies that the value produced by the
// constructor implements one or more other interfaces.
//
// As expects one or more pointers to the implemented interfaces. Values
// produced by constructors will be made available in the container as
// implementations of all of those interfaces.
//
// For example, the following will make the buffer available in the container
// as io.Reader and io.Writer.
//
//   c.Provide(newBuffer, dig.As(new(io.Reader), new(io.Writer)))
//
// That is, the above is equivalent to the following.
//
//   c.Provide(func(...) (*bytes.Buffer, io.Reader, io.Writer) {
//     b := newBuffer(...)
//     return b, b, b
//   })
//
// If used with dig.Name, the type produced by the constructor and the types
// specified with dig.As will all use the same name. For example,
//
//   c.Provide(newFile, dig.As(new(io.Reader)), dig.Name("temp"))
//
// The above is equivalent to the following.
//
//   type Result struct {
//     dig.Out
//
//     File   *os.File  `name:"temp"`
//     Reader io.Reader `name:"temp"`
//   }
//
//   c.Provide(func(...) Result {
//     f := newFile(...)
//     return Result{
//       File: f,
//       Reader: f,
//     }
//   })
//
// This option cannot be provided for constructors which produce result
// objects.
func As(i ...interface{}) ProvideOption {
	return provideOptionFunc(func(opts *provideOptions) {
		opts.As = append(opts.As, i...)
	})
}

// An InvokeOption modifies the default behavior of Invoke. It's included for
// future functionality; currently, there are no concrete implementations.
type InvokeOption interface {
	unimplemented()
}

// Container is a directed acyclic graph of types and their dependencies.
type Container struct {
	// Mapping from key to all the nodes that can provide a value for that
	// key.
	providers map[key][]*node

	// All nodes in the container.
	nodes []*node

	// Values that have already been generated in the container.
	values map[key]reflect.Value

	// Values groups that have already been generated in the container.
	groups map[key][]reflect.Value

	// Source of randomness.
	rand *rand.Rand

	// Flag indicating whether the graph has been checked for cycles.
	isVerifiedAcyclic bool

	// Defer acyclic check on provide until Invoke.
	deferAcyclicVerification bool

	// Name of the container.
	name string

	// Sub graphs of the container.
	children []*Container

	// Parent is the container that spawned this.
	parent *Container

	// Decorator functions of already provided dependencies
	decorators map[key][]*node
}

// containerWriter provides write access to the Container's underlying data
// store.
type containerWriter interface {
	// setValue sets the value with the given name and type in the container.
	// If a value with the same name and type already exists, it will be
	// overwritten.
	setValue(name string, t reflect.Type, v reflect.Value)

	// submitGroupedValue submits a value to the value group with the provided
	// name.
	submitGroupedValue(name string, t reflect.Type, v reflect.Value)
}

// containerStore provides access to the Container's underlying data store.
type containerStore interface {
	containerWriter

	// Returns a slice containing all known types.
	knownTypes() []reflect.Type

	// Retrieves the value with the provided name and type, if any.
	getValue(name string, t reflect.Type) (v reflect.Value, ok bool)

	// Retrieves all values for the provided group and type.
	//
	// The order in which the values are returned is undefined.
	getValueGroup(name string, t reflect.Type) ([]reflect.Value, bool)

	// Returns the providers that can produce a value with the given name and
	// type.
	getValueProviders(name string, t reflect.Type) []provider

	// Returns the providers that can produce values for the given group and
	// type.
	getGroupProviders(name string, t reflect.Type) []provider

	// Returns the decorator list of a particular node
	getDecorators(k key) []*node

	createGraph() *dot.Graph
}

// provider encapsulates a user-provided constructor.
type provider interface {
	// ID is a unique numerical identifier for this provider.
	ID() dot.CtorID

	// Location returns where this constructor was defined.
	Location() *digreflect.Func

	// ParamList returns information about the direct dependencies of this
	// constructor.
	ParamList() paramList

	// ResultList returns information about the values produced by this
	// constructor.
	ResultList() resultList

	// Calls the underlying constructor, reading values from the
	// containerStore as needed.
	//
	// The values produced by this provider should be submitted into the
	// containerStore.
	Call(containerStore) error
}

// New constructs a Container.
func New(opts ...Option) *Container {
	c := &Container{
		providers:  make(map[key][]*node),
		values:     make(map[key]reflect.Value),
		groups:     make(map[key][]reflect.Value),
		decorators: make(map[key][]*node),
		rand:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	for _, opt := range opts {
		opt.applyOption(c)
	}
	return c
}

// DeferAcyclicVerification is an Option to override the default behavior
// of container.Provide, deferring the dependency graph validation to no longer
// run after each call to container.Provide. The container will instead verify
// the graph on first `Invoke`.
//
// Applications adding providers to a container in a tight loop may experience
// performance improvements by initializing the container with this option.
func DeferAcyclicVerification() Option {
	return optionFunc(func(c *Container) {
		c.deferAcyclicVerification = true
	})
}

// Changes the source of randomness for the container.
//
// This will help provide determinism during tests.
func setRand(r *rand.Rand) Option {
	return optionFunc(func(c *Container) {
		c.rand = r
	})
}

// knownTypes returns the types known to this container, including types known
// by its descendants.
func (c *Container) knownTypes() []reflect.Type {
	typeSet := make(map[reflect.Type]struct{}, len(c.providers))
	for k := range c.providers {
		typeSet[k.t] = struct{}{}
	}

	types := make([]reflect.Type, 0, len(typeSet))
	for t := range typeSet {
		types = append(types, t)
	}

	for _, c := range append(c.children) {
		types = append(types, c.knownTypes()...)
	}

	sort.Sort(byTypeName(types))
	return types
}

func (c *Container) getValue(name string, t reflect.Type) (v reflect.Value, ok bool) {
	v, ok = c.values[key{name: name, t: t}]
	return
}

func (c *Container) setValue(name string, t reflect.Type, v reflect.Value) {
	k := key{t: t, name: name}
	c.values[k] = v
}

func (c *Container) getValueGroup(name string, t reflect.Type) ([]reflect.Value, bool) {
	items, ok := c.groups[key{group: name, t: t}]
	if !ok {
		return []reflect.Value{}, ok
	}
	// shuffle the list so users don't rely on the ordering of grouped values
	return shuffledCopy(c.rand, items), true
}

func (c *Container) submitGroupedValue(name string, t reflect.Type, v reflect.Value) {
	k := key{group: name, t: t}
	c.groups[k] = append(c.groups[k], v)
}

func (c *Container) getValueProviders(name string, t reflect.Type) []provider {
	providers := c.getProviders(key{name: name, t: t})

	for _, c := range c.children {
		providers = append(providers, c.getValueProviders(name, t)...)
	}

	return providers
}

func (c *Container) getGroupProviders(name string, t reflect.Type) []provider {
	providers := c.getProviders(key{group: name, t: t})

	for _, c := range c.children {
		providers = append(providers, c.getGroupProviders(name, t)...)
	}

	return providers
}

func (c *Container) getProviders(k key) []provider {
	nodes := c.providers[k]
	providers := make([]provider, len(nodes))
	for i, n := range nodes {
		providers[i] = n
	}
	return providers
}

func (c *Container) getDecorators(k key) []*node {
	p := c
	if _, ok := c.providers[k]; !ok {
		cont := c.children
		for len(cont) > 0 {
			v := cont[0]
			cont = cont[1:]
			if _, ok := v.providers[k]; !ok {
				cont = append(cont, v.children...)
			} else {
				p = v
				break
			}
		}
	} else {
		p = c
	}
	decorators := make([]*node, 0)
	for p != nil {
		if _, ok := p.decorators[k]; ok {
			decorators = append(decorators, p.decorators[k]...)
		}
		p = p.parent
	}
	return decorators
}

func (c *Container) getRoot() *Container {
	if c.parent == nil {
		return c
	}

	return c.parent.getRoot()
}

// Provide teaches the container how to build values of one or more types and
// expresses their dependencies.
//
// The first argument of Provide is a function that accepts zero or more
// parameters and returns one or more results. The function may optionally
// return an error to indicate that it failed to build the value. This
// function will be treated as the constructor for all the types it returns.
// This function will be called AT MOST ONCE when a type produced by it, or a
// type that consumes this function's output, is requested via Invoke. If the
// same types are requested multiple times, the previously produced value will
// be reused.
//
// In addition to accepting constructors that accept dependencies as separate
// arguments and produce results as separate return values, Provide also
// accepts constructors that specify dependencies as dig.In structs and/or
// specify results as dig.Out structs.
func (c *Container) Provide(constructor interface{}, opts ...ProvideOption) error {
	ctype := reflect.TypeOf(constructor)
	if ctype == nil {
		return errors.New("can't provide an untyped nil")
	}
	if ctype.Kind() != reflect.Func {
		return fmt.Errorf("must provide constructor function, got %v (type %v)", constructor, ctype)
	}

	var options provideOptions
	for _, o := range opts {
		o.applyProvideOption(&options)
	}
	if err := options.Validate(); err != nil {
		return err
	}

	if err := c.provide(constructor, options); err != nil {
		return errProvide{
			Func:   digreflect.InspectFunc(constructor),
			Reason: err,
		}
	}
	return nil
}

// Invoke runs the given function after instantiating its dependencies.
//
// Any arguments that the function has are treated as its dependencies. The
// dependencies are instantiated in an unspecified order along with any
// dependencies that they might have.
//
// The function may return an error to indicate failure. The error will be
// returned to the caller as-is.
func (c *Container) Invoke(function interface{}, opts ...InvokeOption) error {
	cp := c.getRoot() // run invoke on root to get access to all the graphs
	ftype := reflect.TypeOf(function)
	if ftype == nil {
		return errors.New("can't invoke an untyped nil")
	}
	if ftype.Kind() != reflect.Func {
		return fmt.Errorf("can't invoke non-function %v (type %v)", function, ftype)
	}

	pl, err := newParamList(ftype)
	if err != nil {
		return err
	}

	if err := shallowCheckDependencies(cp, pl); err != nil {
		return errMissingDependencies{
			Func:   digreflect.InspectFunc(function),
			Reason: err,
		}
	}

	if !cp.isVerifiedAcyclic {
		if err := cp.verifyAcyclic(); err != nil {
			return err
		}
	}

	args, err := pl.BuildList(cp)
	if err != nil {
		return errArgumentsFailed{
			Func:   digreflect.InspectFunc(function),
			Reason: err,
		}
	}

	returned := reflect.ValueOf(function).Call(args)
	if len(returned) == 0 {
		return nil
	}
	if last := returned[len(returned)-1]; isError(last.Type()) {
		if err, _ := last.Interface().(error); err != nil {
			return err
		}
	}
	return nil
}

func (c *Container) Decorate(decorator interface{}, opts ...ProvideOption) error {
	dtype := reflect.TypeOf(decorator)
	if dtype == nil {
		return errors.New("can't decorate with an untyped nil")
	}
	if dtype.Kind() != reflect.Func {
		return fmt.Errorf("can't call non-function %v (type %v)", decorator, dtype)
	}

	var options provideOptions
	for _, o := range opts {
		o.applyProvideOption(&options)
	}
	if err := options.Validate(); err != nil {
		return err
	}

	if err := c.decorate(decorator, options); err != nil {
		return errConstructorFailed{
			Func:   digreflect.InspectFunc(decorator),
			Reason: err,
		}
	}
	return nil
}

// Child returns a named child of this container. The child container has
// full access to the parent's types, and any types provided to the child
// will be made available to the parent.
//
// The name of the child is for observability purposes only. As such, it
// does not have to be unique across different children of the container.
func (c *Container) Child(name string) *Container {
	child := &Container{
		providers:  make(map[key][]*node),
		values:     make(map[key]reflect.Value),
		groups:     make(map[key][]reflect.Value),
		decorators: make(map[key][]*node),
		rand:       c.rand,
		name:       name,
		parent:     c,
	}

	c.children = append(c.children, child)

	return child
}

func (c *Container) verifyAcyclic() error {
	visited := make(map[key]struct{})
	for _, n := range c.nodes {
		if err := detectCycles(n, c, nil /* path */, visited); err != nil {
			return errWrapf(err, "cycle detected in dependency graph")
		}
	}

	c.isVerifiedAcyclic = true
	return nil
}

func (c *Container) provide(ctor interface{}, opts provideOptions) error {
	n, err := newNode(
		ctor,
		nodeOptions{
			ResultName:  opts.Name,
			ResultGroup: opts.Group,
			ResultAs:    opts.As,
		},
	)
	if err != nil {
		return err
	}

	keys, err := c.findAndValidateResults(n)
	if err != nil {
		return err
	}

	ctype := reflect.TypeOf(ctor)
	if len(keys) == 0 {
		return fmt.Errorf("%v must provide at least one non-error type", ctype)
	}

	for k := range keys {
		c.isVerifiedAcyclic = false
		oldProviders := c.providers[k]
		c.providers[k] = append(c.providers[k], n)

		if c.deferAcyclicVerification {
			continue
		}
		if err := verifyAcyclic(c.getRoot(), n, k); err != nil {
			c.providers[k] = oldProviders
			return err
		}
		c.isVerifiedAcyclic = true
	}

	c.nodes = append(c.nodes, n)

	return nil
}

// Builds a collection of all result types produced by this node.
func (c *Container) findAndValidateResults(n *node) (map[key]struct{}, error) {
	var err error
	keyPaths := make(map[key]string)
	walkResult(n.ResultList(), connectionVisitor{
		c:        c.getRoot(),
		n:        n,
		err:      &err,
		keyPaths: keyPaths,
	})

	if err != nil {
		return nil, err
	}

	keys := make(map[key]struct{}, len(keyPaths))
	for k := range keyPaths {
		keys[k] = struct{}{}
	}
	return keys, nil
}

func (c *Container) decorate(dtor interface{}, opts provideOptions) error {
	n, err := newNode(
		dtor,
		nodeOptions{
			ResultName:  opts.Name,
			ResultGroup: opts.Group,
			ResultAs:    opts.As,
		},
	)
	if err != nil {
		return err
	}

	dtype := reflect.TypeOf(dtor)

	// Check if all the result types exist among the input types
	inTypes := make(map[key]struct{})
	for i := 0; i < dtype.NumIn(); i++ {
		in := dtype.In(i)
		if IsIn(in) {
			for j := 0; j < in.NumField(); j++ {
				t := in.Field(j).Type
				//Exclude embedded In type
				if IsIn(t) {
					continue
				}
				name := in.Field(j).Tag.Get(_nameTag)
				group := in.Field(j).Tag.Get(_groupTag)
				if name != "" && group != "" {
					return errors.New("cannot use name tags and group tags together")
				}
				if group != "" {
					if _, ok := inTypes[key{t.Elem(), name, group}]; ok {
						return fmt.Errorf("cannot provide %v multple times in decorator", t)
					}
					inTypes[key{t.Elem(), name, group}] = struct{}{}
				} else {
					if _, ok := inTypes[key{t, name, group}]; ok {
						return fmt.Errorf("cannot provide %v multple times in decorator", t)
					}
					inTypes[key{t, name, group}] = struct{}{}
				}
			}
		} else {
			inTypes[key{t: in}] = struct{}{}
		}
	}
	outTypes := make(map[key]struct{})
	for i := 0; i < dtype.NumOut(); i++ {
		out := dtype.Out(i)
		if IsOut(out) {
			for j := 0; j < out.NumField(); j++ {
				t := out.Field(j).Type
				//Exclude embedded Out type
				if IsOut(t) {
					continue
				}
				name := out.Field(j).Tag.Get(_nameTag)
				group := out.Field(j).Tag.Get(_groupTag)
				if name != "" && group != "" {
					return errors.New("cannot use name tags and group tags together")
				}
				if _, ok := outTypes[key{t, name, group}]; ok {
					return fmt.Errorf("cannot provide %v multple times in decorator", t)
				}
				outTypes[key{t, name, group}] = struct{}{}
			}
		} else {
			outTypes[key{t: out}] = struct{}{}
		}
	}

	for k := range outTypes {
		if _, ok := inTypes[k]; !ok {
			return errors.New("the result types, with the exception of error, must be present among the input parameters")
		}
		delete(inTypes, k)
	}

	params := []param{}
	for k := range inTypes {
		if k.group != "" {
			params = append(params, paramGroupedSlice{k.group, reflect.SliceOf(k.t)})
		} else {
			params = append(params, paramSingle{
				Name: k.name,
				Type: k.t,
			})
		}
	}

	for k := range outTypes {
		found := false
		// Checking for the decorator output's existence in the sub graph with the
		// current container as root.
		if _, ok := c.providers[k]; !ok {
			var cont []*Container
			cont = append(cont, c.children...)
			for !found && !(len(cont) == 0) {
				v := cont[0]
				cont = cont[1:]
				if _, ok := v.providers[k]; !ok {
					cont = append(cont, v.children...)
				} else {
					found = true
				}
			}
		} else {
			found = true
		}
		if !found {
			return errors.New("decorator must be declared in the scope of the node's container or its ancestors')")
		}

		if len(params) > 0 {
			c.isVerifiedAcyclic = false
			oldParams := n.paramList.Params
			oldProviders := c.providers[k]
			for _, p := range c.providers[k] {
				params = append(params, p.paramList.Params...)
			}
			n.paramList.Params = params
			c.providers[k] = append([]*node{n}, c.providers[k]...)
			if err := verifyAcyclic(c.getRoot(), n, k); err != nil {
				c.providers[k] = oldProviders
				return err
			}
			c.providers[k] = oldProviders
			n.paramList.Params = oldParams
			c.isVerifiedAcyclic = true
		}
		c.decorators[k] = append(c.decorators[k], n)
	}
	return nil
}

// Visits the results of a node and compiles a collection of all the keys
// produced by that node.
type connectionVisitor struct {
	c *Container
	n *node

	// If this points to a non-nil value, we've already encountered an error
	// and should stop traversing.
	err *error

	// Map of keys provided to path that provided this. The path is a string
	// documenting which positional return value or dig.Out attribute is
	// providing this particular key.
	//
	// For example, "[0].Foo" indicates that the value was provided by the Foo
	// attribute of the dig.Out returned as the first result of the
	// constructor.
	keyPaths map[key]string

	// We track the path to the current result here. For example, this will
	// be, ["[1]", "Foo", "Bar"] when we're visiting Bar in,
	//
	//   func() (io.Writer, struct {
	//     dig.Out
	//
	//     Foo struct {
	//       dig.Out
	//
	//       Bar io.Reader
	//     }
	//   })
	currentResultPath []string
}

func (cv connectionVisitor) AnnotateWithField(f resultObjectField) resultVisitor {
	cv.currentResultPath = append(cv.currentResultPath, f.FieldName)
	return cv
}

func (cv connectionVisitor) AnnotateWithPosition(i int) resultVisitor {
	cv.currentResultPath = append(cv.currentResultPath, fmt.Sprintf("[%d]", i))
	return cv
}

func (cv connectionVisitor) Visit(res result) resultVisitor {
	// Already failed. Stop looking.
	if *cv.err != nil {
		return nil
	}

	path := strings.Join(cv.currentResultPath, ".")

	switch r := res.(type) {

	case resultSingle:
		k := key{name: r.Name, t: r.Type}
		if err := cv.checkKey(k, path); err != nil {
			*cv.err = err
			return nil
		}

		cv.keyPaths[k] = path
		for _, asType := range r.As {
			k := key{name: r.Name, t: asType}
			if err := cv.checkKey(k, path); err != nil {
				*cv.err = err
				return nil
			}
			cv.keyPaths[k] = path
		}

	case resultGrouped:
		// we don't really care about the path for this since conflicts are
		// okay for group results. We'll track it for the sake of having a
		// value there.
		k := key{group: r.Group, t: r.Type}
		cv.keyPaths[k] = path
	}

	return cv
}

func (cv connectionVisitor) checkKey(k key, path string) error {
	if conflict, ok := cv.keyPaths[k]; ok {
		return fmt.Errorf(
			"cannot provide %v from %v: already provided by %v",
			k, path, conflict)
	}
	if ps := cv.c.getValueProviders(k.name, k.t); len(ps) > 0 {
		cons := make([]string, len(ps))
		for i, p := range ps {
			cons[i] = fmt.Sprint(p.Location())
		}

		return fmt.Errorf(
			"cannot provide %v from %v: already provided by %v",
			k, path, strings.Join(cons, "; "))
	}

	return nil
}

// node is a node in the dependency graph. Each node maps to a single
// constructor provided by the user.
//
// Nodes can produce zero or more values that they store into the container.
// For the Provide path, we verify that nodes produce at least one value,
// otherwise the function will never be called.
type node struct {
	ctor  interface{}
	ctype reflect.Type

	// Location where this function was defined.
	location *digreflect.Func

	// id uniquely identifies the constructor that produces a node.
	id dot.CtorID

	// Whether the constructor owned by this node was already called.
	called bool

	// Type information about constructor parameters.
	paramList paramList

	// Type information about constructor results.
	resultList resultList
}

type nodeOptions struct {
	// If specified, all values produced by this node have the provided name
	// belong to the specified value group or implement any of the interfaces.
	ResultName  string
	ResultGroup string
	ResultAs    []interface{}
}

func newNode(ctor interface{}, opts nodeOptions) (*node, error) {
	cval := reflect.ValueOf(ctor)
	ctype := cval.Type()
	cptr := cval.Pointer()

	params, err := newParamList(ctype)
	if err != nil {
		return nil, err
	}

	results, err := newResultList(
		ctype,
		resultOptions{
			Name:  opts.ResultName,
			Group: opts.ResultGroup,
			As:    opts.ResultAs,
		},
	)
	if err != nil {
		return nil, err
	}

	return &node{
		ctor:       ctor,
		ctype:      ctype,
		location:   digreflect.InspectFunc(ctor),
		id:         dot.CtorID(cptr),
		paramList:  params,
		resultList: results,
	}, err
}

func (n *node) Location() *digreflect.Func { return n.location }
func (n *node) ParamList() paramList       { return n.paramList }
func (n *node) ResultList() resultList     { return n.resultList }
func (n *node) ID() dot.CtorID             { return n.id }

// Call calls this node's constructor if it hasn't already been called and
// injects any values produced by it into the provided container.
func (n *node) Call(c containerStore) error {
	if n.called {
		return nil
	}
	if err := shallowCheckDependencies(c, n.paramList); err != nil {
		return errMissingDependencies{
			Func:   n.location,
			Reason: err,
		}
	}
	args, err := n.paramList.BuildList(c)
	if err != nil {
		return errArgumentsFailed{
			Func:   n.location,
			Reason: err,
		}
	}
	if n.called {
		return nil
	}
	receiver := newStagingContainerWriter()
	results := reflect.ValueOf(n.ctor).Call(args)
	if err := n.resultList.ExtractList(receiver, results); err != nil {
		return errConstructorFailed{Func: n.location, Reason: err}
	}
	receiver.Commit(c)
	n.called = true
	return nil
}

// Checks if a field of an In struct is optional.
func isFieldOptional(f reflect.StructField) (bool, error) {
	tag := f.Tag.Get(_optionalTag)
	if tag == "" {
		return false, nil
	}

	optional, err := strconv.ParseBool(tag)
	if err != nil {
		err = errWrapf(err,
			"invalid value %q for %q tag on field %v",
			tag, _optionalTag, f.Name)
	}

	return optional, err
}

// Checks that all direct dependencies of the provided param are present in
// the container. Returns an error if not.
func shallowCheckDependencies(c containerStore, p param) error {
	var missing errMissingManyTypes
	var addMissingNodes []*dot.Param
	walkParam(p, paramVisitorFunc(func(p param) bool {
		ps, ok := p.(paramSingle)
		if !ok {
			return true
		}

		if ns := c.getValueProviders(ps.Name, ps.Type); len(ns) == 0 && !ps.Optional {
			missing = append(missing, newErrMissingType(c, key{name: ps.Name, t: ps.Type}))
			addMissingNodes = append(addMissingNodes, ps.DotParam()...)
		}

		return true
	}))

	if len(missing) > 0 {
		return missing
	}
	return nil
}

// stagingContainerWriter is a containerWriter that records the changes that
// would be made to a containerWriter and defers them until Commit is called.
type stagingContainerWriter struct {
	values      map[key]reflect.Value
	groups      map[key][]reflect.Value
	isDecorated map[key]bool
}

var _ containerWriter = (*stagingContainerWriter)(nil)

func newStagingContainerWriter() *stagingContainerWriter {
	return &stagingContainerWriter{
		values: make(map[key]reflect.Value),
		groups: make(map[key][]reflect.Value),
	}
}

func (sr *stagingContainerWriter) setValue(name string, t reflect.Type, v reflect.Value) {
	sr.values[key{t: t, name: name}] = v
}

func (sr *stagingContainerWriter) submitGroupedValue(group string, t reflect.Type, v reflect.Value) {
	k := key{t: t, group: group}
	sr.groups[k] = append(sr.groups[k], v)
}

// Commit commits the received results to the provided containerWriter.
func (sr *stagingContainerWriter) Commit(cw containerWriter) {
	for k, v := range sr.values {
		cw.setValue(k.name, k.t, v)
	}

	for k, vs := range sr.groups {
		for _, v := range vs {
			cw.submitGroupedValue(k.group, k.t, v)
		}
	}
}

type byTypeName []reflect.Type

func (bs byTypeName) Len() int {
	return len(bs)
}

func (bs byTypeName) Less(i int, j int) bool {
	return fmt.Sprint(bs[i]) < fmt.Sprint(bs[j])
}

func (bs byTypeName) Swap(i int, j int) {
	bs[i], bs[j] = bs[j], bs[i]
}

func shuffledCopy(rand *rand.Rand, items []reflect.Value) []reflect.Value {
	newItems := make([]reflect.Value, len(items))
	for i, j := range rand.Perm(len(items)) {
		newItems[i] = items[j]
	}
	return newItems
}
