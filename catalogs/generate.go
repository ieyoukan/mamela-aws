package catalogs

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// Generate makes the catalogs from AWS's models (by model name, as models.lock.json names them)
// and the overlay. It fails, saying every problem, when an operation or a parameter of the overlay
// is not in the model (or is deprecated), when no rule or override classifies an operation, when
// the model marks an operation read-only and it is not classified read, or when an override does
// not override anything.
func Generate(models map[string]*Model) (map[string]protocol.Catalog, error) {
	return generate(models, APIs, Overrides)
}

func generate(models map[string]*Model, apis []API, overrides map[string]map[string]Override) (map[string]protocol.Catalog, error) {
	var problems errs
	out := map[string]protocol.Catalog{}
	used := map[string]map[string]bool{}
	for _, a := range apis {
		m := models[a.Model]
		if m == nil {
			problems.add("%s: no model %q", a.Name, a.Model)
			continue
		}
		used[a.Name] = map[string]bool{}
		c := protocol.Catalog{API: a.Name}
		seen := map[string]bool{}
		for _, op := range a.Ops {
			if seen[op.Name] {
				problems.add("%s %s: twice in the overlay", a.Name, op.Name)
				continue
			}
			seen[op.Name] = true
			used[a.Name][op.Name] = true
			if !m.Has(op.Name) {
				problems.add("%s: the model (%s %s) has no operation %s", a.Name, m.Service, m.Version, op.Name)
				continue
			}
			if d := m.Deprecated(op.Name); d != "" {
				problems.add("%s %s: deprecated in the model: %s", a.Name, op.Name, d)
			}
			effect, err := classify(overrides, a.Name, op.Name)
			if err != nil {
				problems = append(problems, err)
				continue
			}
			if m.ReadOnly(op.Name) && effect != protocol.EffectRead {
				problems.add("%s %s: the model marks it read-only, and it is classified %s", a.Name, op.Name, effect)
			}
			for _, p := range op.Params {
				if err := m.Param(op.Name, p); err != nil {
					problems.add("%s: %v", a.Name, err)
				}
			}
			e, err := entry(m, op, effect)
			if err != nil {
				problems.add("%s %s: %v", a.Name, op.Name, err)
				continue
			}
			c.Entries = append(c.Entries, e)
		}
		if err := c.Check(); err != nil {
			problems = append(problems, err)
		}
		out[a.Name] = c
	}
	for _, api := range sortedKeys(overrides) {
		ops := overrides[api]
		for _, op := range slices.Sorted(maps.Keys(ops)) {
			o := ops[op]
			if !used[api][op] {
				problems.add("override of %s %s: not an operation of the overlay", api, op)
			}
			if e, ok := byRule(op); ok && e == o.Effect {
				problems.add("override of %s %s: the rules already classify it %s", api, op, e)
			}
			if o.Why == "" {
				problems.add("override of %s %s: says no why", api, op)
			}
		}
	}
	return out, problems.err()
}

// entry is the catalog entry of an operation: POST /, its parameters where the protocol puts
// them, and the call told apart by Action and Version (query) or X-Amz-Target (JSON).
func entry(m *Model, op Op, effect protocol.Effect) (protocol.Entry, error) {
	e := protocol.Entry{Method: "POST", Path: "/", Effect: effect, Bind: op.Bind, Values: op.Values, Measures: op.Measures}
	if m.Query() {
		e.Form = append([]string{"Action", "Version"}, op.Params...)
		e.Fixed = map[string]string{"Action": op.Name, "Version": m.Version}
		for k, v := range op.Fixed {
			if !slices.Contains(op.Params, k) {
				return e, fmt.Errorf("fixes %s, which is not one of its parameters", k)
			}
			e.Fixed[k] = v
		}
		return e, nil
	}
	if len(op.Fixed) > 0 {
		return e, fmt.Errorf("a JSON API's call is told apart by its X-Amz-Target alone; it fixes no member")
	}
	e.Header = []string{"X-Amz-Target"}
	e.JSON = op.Params
	e.Fixed = map[string]string{"X-Amz-Target": m.Target(op.Name)}
	return e, nil
}

// Encode writes a catalog file: the schema, then the catalog, indented and deterministic.
func Encode(c protocol.Catalog) ([]byte, error) {
	c.Schema = protocol.CatalogSchema
	if err := c.Check(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(c, json.Deterministic(true), jsontext.Multiline(true), jsontext.WithIndent("  "))
	return append(data, '\n'), err
}

// CheckCalls checks calls a provider declares against the models: each is an operation of its
// API's model (its Action, or X-Amz-Target), with the model's version, and each of its parameters
// exists there. models are by API name.
func CheckCalls(models map[string]*Model, calls []protocol.Call) error {
	var problems errs
	for _, c := range calls {
		m := models[c.API]
		if m == nil {
			problems.add("call %s %s %v: no model of API %q", c.Method, c.Path, c.Fixed, c.API)
			continue
		}
		var op string
		var params []string
		if m.Query() {
			op = c.Fixed["Action"]
			if v := c.Fixed["Version"]; v != m.Version {
				problems.add("%s %s: version %q, the model's is %s", c.API, op, v, m.Version)
			}
			params = slices.DeleteFunc(slices.Clone(c.Form), func(p string) bool { return p == "Action" || p == "Version" })
			if len(c.JSON) > 0 || len(c.Header) > 0 || len(c.Query) > 0 {
				problems.add("%s %s: a query API reads only the form", c.API, op)
			}
		} else {
			t := c.Fixed["X-Amz-Target"]
			svc, name, _ := strings.Cut(t, ".")
			if svc != m.Service {
				problems.add("%s: X-Amz-Target %q is not of service %s", c.API, t, m.Service)
			}
			op, params = name, c.JSON
			if len(c.Form) > 0 || len(c.Query) > 0 || !slices.Equal(c.Header, []string{"X-Amz-Target"}) {
				problems.add("%s %s: a JSON API reads its X-Amz-Target header and the JSON body only", c.API, op)
			}
		}
		if !m.Has(op) {
			problems.add("%s: the model has no operation %q", c.API, op)
			continue
		}
		for _, p := range params {
			if err := m.Param(op, p); err != nil {
				problems.add("%s: %v", c.API, err)
			}
		}
	}
	return problems.err()
}

// Consistent checks a shipped catalog against the overlay and the rules, without a model: the
// same operations in the same order, each with the overlay's parameters, bind, values, measures
// and fixed values, and the effect the rules give. Generate (with the models) checks the rest.
func Consistent(a API, c protocol.Catalog) error {
	var problems errs
	if c.API != a.Name {
		problems.add("catalog %q is not %s", c.API, a.Name)
	}
	if len(c.Entries) != len(a.Ops) {
		problems.add("%s: %d entries, the overlay has %d operations", a.Name, len(c.Entries), len(a.Ops))
		return problems.err()
	}
	for i, op := range a.Ops {
		e := c.Entries[i]
		name := e.Fixed["Action"]
		params := slices.DeleteFunc(slices.Clone(e.Form), func(p string) bool { return p == "Action" || p == "Version" })
		if t := e.Fixed["X-Amz-Target"]; t != "" {
			_, name, _ = strings.Cut(t, ".")
			params = e.JSON
		}
		effect, err := Classify(a.Name, op.Name)
		fixed := maps.Clone(e.Fixed)
		delete(fixed, "Action")
		delete(fixed, "Version")
		delete(fixed, "X-Amz-Target")
		switch {
		case name != op.Name:
			problems.add("%s entry %d is %s, the overlay's %s", a.Name, i, name, op.Name)
		case err != nil || e.Effect != effect:
			problems.add("%s %s: effect %s, the rules give %s (%v)", a.Name, op.Name, e.Effect, effect, err)
		case !slices.Equal(params, op.Params) || !slices.Equal(e.Bind, op.Bind) || !maps.EqualFunc(e.Values, op.Values, slices.Equal) ||
			!slices.EqualFunc(e.Measures, op.Measures, func(x, y protocol.Measure) bool {
				return x.Param == y.Param && x.Name == y.Name && x.Format == y.Format && maps.Equal(x.Table, y.Table)
			}) || len(fixed) != len(op.Fixed) || len(op.Fixed) > 0 && !maps.Equal(fixed, op.Fixed):
			problems.add("%s %s: its parameters, bind, values, measures or fixed values are not the overlay's", a.Name, op.Name)
		}
	}
	return problems.err()
}
