package catalogs

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Model is one AWS service's Smithy model (the JSON AST AWS publishes in
// github.com/aws/api-models-aws): what the catalogs are checked against and derived from. The
// generator reads from it the service's protocol (where each parameter goes), its version (EC2's
// and IAM's Version), the X-Amz-Target of a JSON API's calls, and whether each operation and each
// parameter exists as the protocol serializes it ("Filter.1.Value.1", "Tags.member.1.Key").
type Model struct {
	// Service is the service shape's name ("AmazonEC2ContainerServiceV20141113"): a JSON API's
	// X-Amz-Target is "<Service>.<Operation>".
	Service string
	// Version is the service's API version ("2016-11-15").
	Version string
	// Protocol is the service's AWS protocol: ProtocolEC2Query, ProtocolAWSQuery or
	// ProtocolAWSJSON (1.0 and 1.1).
	Protocol string

	shapes map[string]shape
	ops    map[string]string // operation name -> shape ID
}

// The AWS protocols the generator knows.
const (
	ProtocolEC2Query = "ec2Query"   // EC2: a form, lists always flattened (Name.1)
	ProtocolAWSQuery = "awsQuery"   // IAM: a form, lists as Name.member.1 unless flattened
	ProtocolAWSJSON  = "awsJson1_1" // ECS, CloudWatch Logs, SSM: a JSON body, X-Amz-Target
)

type target struct {
	Target string `json:"target"`
}

type member struct {
	Target string                    `json:"target"`
	Traits map[string]jsontext.Value `json:"traits"`
}

type shape struct {
	Type                 string                    `json:"type"`
	Version              string                    `json:"version"`
	Members              map[string]member         `json:"members"`
	Member               *member                   `json:"member"`
	Input                *target                   `json:"input"`
	Operations           []target                  `json:"operations"`
	CollectionOperations []target                  `json:"collectionOperations"`
	Resources            []target                  `json:"resources"`
	Create               *target                   `json:"create"`
	Put                  *target                   `json:"put"`
	Read                 *target                   `json:"read"`
	Update               *target                   `json:"update"`
	Delete               *target                   `json:"delete"`
	List                 *target                   `json:"list"`
	Traits               map[string]jsontext.Value `json:"traits"`
}

// LoadModel reads a Smithy JSON AST model of one service.
func LoadModel(data []byte) (*Model, error) {
	var ast struct {
		Smithy string           `json:"smithy"`
		Shapes map[string]shape `json:"shapes"`
	}
	if err := json.Unmarshal(data, &ast); err != nil {
		return nil, fmt.Errorf("not a Smithy JSON model: %w", err)
	}
	if !strings.HasPrefix(ast.Smithy, "2.") {
		return nil, fmt.Errorf("Smithy IDL version %q: the generator reads 2.x", ast.Smithy)
	}
	m := &Model{shapes: ast.Shapes, ops: map[string]string{}}
	var services []string
	for id, s := range ast.Shapes {
		if s.Type == "service" {
			services = append(services, id)
		}
	}
	if len(services) != 1 {
		return nil, fmt.Errorf("the model has %d services; one is expected", len(services))
	}
	id := services[0]
	svc := ast.Shapes[id]
	_, m.Service, _ = strings.Cut(id, "#")
	m.Version = svc.Version
	var protocols []string
	for t := range svc.Traits {
		if p, ok := strings.CutPrefix(t, "aws.protocols#"); ok && !strings.HasSuffix(p, "Compatible") {
			protocols = append(protocols, p)
		}
	}
	slices.Sort(protocols)
	switch {
	case slices.Equal(protocols, []string{"ec2Query"}):
		m.Protocol = ProtocolEC2Query
	case slices.Equal(protocols, []string{"awsQuery"}):
		m.Protocol = ProtocolAWSQuery
	case slices.Equal(protocols, []string{"awsJson1_1"}), slices.Equal(protocols, []string{"awsJson1_0"}):
		m.Protocol = ProtocolAWSJSON
	default:
		return nil, fmt.Errorf("service %s speaks %v: the generator knows ec2Query, awsQuery and awsJson1_x, one of them", m.Service, protocols)
	}
	if m.Version == "" {
		return nil, fmt.Errorf("service %s has no version", m.Service)
	}
	// The operations: the service's own, and those of its resources (ECS has them).
	var walk func(id string, seen map[string]bool) error
	walk = func(id string, seen map[string]bool) error {
		if seen[id] {
			return nil
		}
		seen[id] = true
		s, ok := ast.Shapes[id]
		if !ok {
			return fmt.Errorf("shape %s is not in the model", id)
		}
		var ops []target
		ops = append(ops, s.Operations...)
		ops = append(ops, s.CollectionOperations...)
		for _, t := range []*target{s.Create, s.Put, s.Read, s.Update, s.Delete, s.List} {
			if t != nil {
				ops = append(ops, *t)
			}
		}
		for _, o := range ops {
			_, name, _ := strings.Cut(o.Target, "#")
			if prev, dup := m.ops[name]; dup && prev != o.Target {
				return fmt.Errorf("operation %s is %s and %s", name, prev, o.Target)
			}
			m.ops[name] = o.Target
		}
		for _, r := range s.Resources {
			if err := walk(r.Target, seen); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(id, map[string]bool{}); err != nil {
		return nil, err
	}
	return m, nil
}

// Target is the X-Amz-Target of an operation of a JSON API ("Logs_20140328.CreateLogGroup").
func (m *Model) Target(op string) string { return m.Service + "." + op }

// Query says whether the service's requests are forms (EC2, IAM) rather than JSON.
func (m *Model) Query() bool { return m.Protocol != ProtocolAWSJSON }

func (m *Model) operation(op string) (shape, error) {
	id, ok := m.ops[op]
	if !ok {
		return shape{}, fmt.Errorf("%s has no operation %s", m.Service, op)
	}
	s := m.shapes[id]
	if s.Type != "operation" {
		return shape{}, fmt.Errorf("%s: %s is a %s, not an operation", m.Service, op, s.Type)
	}
	return s, nil
}

// Has says whether the service has the operation.
func (m *Model) Has(op string) bool {
	_, err := m.operation(op)
	return err == nil
}

// ReadOnly says whether the model marks the operation @readonly (it changes nothing).
func (m *Model) ReadOnly(op string) bool {
	s, err := m.operation(op)
	return err == nil && s.Traits["smithy.api#readonly"] != nil
}

// Deprecated is why the model deprecates the operation ("" when it does not).
func (m *Model) Deprecated(op string) string {
	s, err := m.operation(op)
	if err != nil {
		return ""
	}
	return deprecation(s.Traits)
}

func deprecation(traits map[string]jsontext.Value) string {
	d, ok := traits["smithy.api#deprecated"]
	if !ok {
		return ""
	}
	var v struct {
		Message string `json:"message"`
		Since   string `json:"since"`
	}
	json.Unmarshal(d, &v)
	if v.Message == "" {
		v.Message = "deprecated"
	}
	if v.Since != "" {
		v.Message += " (since " + v.Since + ")"
	}
	return v.Message
}

// Param checks that a parameter of an operation exists as the service's protocol serializes it:
// a JSON API's top-level member ("containerDefinitions"); a query API's path through structures
// and lists ("TagSpecification.1.Tag.2.Key" for EC2, "Tags.member.1.Key" for IAM), which ends at
// a value. A deprecated parameter is refused.
func (m *Model) Param(op, name string) error {
	s, err := m.operation(op)
	if err != nil {
		return err
	}
	if s.Input == nil {
		return fmt.Errorf("%s %s takes no input, and no %s", m.Service, op, name)
	}
	in, ok := m.shapes[s.Input.Target]
	if !ok || in.Type != "structure" {
		return fmt.Errorf("%s %s: its input %s is not a structure", m.Service, op, s.Input.Target)
	}
	if m.Protocol == ProtocolAWSJSON {
		mb, ok := in.Members[name]
		if !ok {
			return fmt.Errorf("%s %s has no member %q", m.Service, op, name)
		}
		if d := deprecation(mb.Traits); d != "" {
			return fmt.Errorf("%s %s: member %s is deprecated: %s", m.Service, op, name, d)
		}
		return nil
	}
	segs := strings.Split(name, ".")
	cur, i := in, 0
	path := op
	for {
		switch cur.Type {
		case "structure", "union":
			if i == len(segs) {
				return fmt.Errorf("%s %s: %s is a structure, not a value", m.Service, op, name)
			}
			found := ""
			var mb member
			for _, n := range sortedKeys(cur.Members) {
				if m.queryName(n, cur.Members[n]) == segs[i] {
					found, mb = n, cur.Members[n]
					break
				}
			}
			if found == "" {
				return fmt.Errorf("%s %s: %s has no parameter %q (of %s)", m.Service, op, path, segs[i], name)
			}
			if d := deprecation(mb.Traits); d != "" {
				return fmt.Errorf("%s %s: %s.%s is deprecated: %s", m.Service, op, path, found, d)
			}
			path += "." + found
			i++
			next, err := m.shapeOf(mb.Target)
			if err != nil {
				return err
			}
			if next.Type == "list" || next.Type == "set" {
				// awsQuery: Name.member.N, unless the member is flattened (Name.N); ec2Query:
				// always flattened.
				if m.Protocol == ProtocolAWSQuery && mb.Traits["smithy.api#xmlFlattened"] == nil {
					want := "member"
					if next.Member != nil {
						if x := traitString(next.Member.Traits, "smithy.api#xmlName"); x != "" {
							want = x
						}
					}
					if i == len(segs) || segs[i] != want {
						return fmt.Errorf("%s %s: list %s is written %s.%s.N", m.Service, op, path, segs[i-1], want)
					}
					i++
				}
				if i == len(segs) || !positive(segs[i]) {
					return fmt.Errorf("%s %s: list %s is written with an index from 1 (%s)", m.Service, op, path, name)
				}
				i++
				if next.Member == nil {
					return fmt.Errorf("%s %s: list %s has no member shape", m.Service, op, path)
				}
				if next, err = m.shapeOf(next.Member.Target); err != nil {
					return err
				}
			}
			cur = next
		case "map":
			return fmt.Errorf("%s %s: %s is a map, which the generator does not write as a query (%s)", m.Service, op, path, name)
		default: // a value
			if i != len(segs) {
				return fmt.Errorf("%s %s: %s is a value; %q follows it in %s", m.Service, op, path, strings.Join(segs[i:], "."), name)
			}
			return nil
		}
	}
}

// queryName is how a query protocol names a structure's member.
func (m *Model) queryName(name string, mb member) string {
	if m.Protocol == ProtocolEC2Query {
		if v := traitString(mb.Traits, "aws.protocols#ec2QueryName"); v != "" {
			return v
		}
		if v := traitString(mb.Traits, "smithy.api#xmlName"); v != "" {
			return upperFirst(v)
		}
		return upperFirst(name)
	}
	if v := traitString(mb.Traits, "smithy.api#xmlName"); v != "" {
		return v
	}
	return name
}

// shapeOf is a shape by ID; Smithy's prelude shapes (smithy.api#String) are values.
func (m *Model) shapeOf(id string) (shape, error) {
	if s, ok := m.shapes[id]; ok {
		return s, nil
	}
	if strings.HasPrefix(id, "smithy.api#") {
		return shape{Type: "value"}, nil
	}
	return shape{}, fmt.Errorf("shape %s is not in the model", id)
}

func traitString(traits map[string]jsontext.Value, k string) string {
	var s string
	if v, ok := traits[k]; ok {
		json.Unmarshal(v, &s)
	}
	return s
}

func upperFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:]
}

func positive(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && strconv.Itoa(n) == s
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// errs collects problems, to report them all at once.
type errs []error

func (e *errs) add(format string, args ...any) { *e = append(*e, fmt.Errorf(format, args...)) }

func (e errs) err() error { return errors.Join(e...) }
