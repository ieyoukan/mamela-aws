package catalogs

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// The effect of an operation comes from its name (AWS names its operations by a verb), by these
// rules, in this order; an operation no rule covers is not classified, and the generator fails:
// a new verb is reviewed and added here, never guessed.
var rules = []struct {
	verbs  []string
	effect protocol.Effect
}{
	{[]string{"Describe", "Get", "List"}, protocol.EffectRead},
	{[]string{"Create", "Run", "Register"}, protocol.EffectCreate},
	{[]string{"Delete", "Terminate", "Deregister"}, protocol.EffectLoseData},
	{[]string{"Start", "Stop", "Reboot"}, protocol.EffectRestart},
	{[]string{"Modify", "Update", "Put", "Attach", "Detach", "Authorize", "Revoke", "Associate", "Disassociate",
		"Tag", "Untag", "Add", "Remove"}, protocol.EffectChange},
}

// Override is a reviewed effect that the rules do not give.
type Override struct {
	Effect protocol.Effect
	Why    string
}

// Overrides are the reviewed exceptions to the rules, by API and operation. Each must differ from
// what the rules say (the generator refuses one that does not), and name an operation of the
// catalog.
var Overrides = map[string]map[string]Override{
	"ec2": {
		"CreateTags":  {protocol.EffectChange, "it tags an existing object; it creates nothing"},
		"CreateRoute": {protocol.EffectChange, "it adds a route to an existing route table, which is the object it changes"},
	},
	"ecs": {
		"DeregisterTaskDefinition": {protocol.EffectChange, "ECS keeps the revision (INACTIVE): services that use it keep running, and it can still be described"},
	},
}

// Classify is the effect of an API's operation: the reviewed override, or the rule of its verb.
func Classify(api, op string) (protocol.Effect, error) {
	return classify(Overrides, api, op)
}

func classify(overrides map[string]map[string]Override, api, op string) (protocol.Effect, error) {
	if o, ok := overrides[api][op]; ok {
		return o.Effect, nil
	}
	if e, ok := byRule(op); ok {
		return e, nil
	}
	return "", fmt.Errorf("%s %s: no rule classifies it (its verb is none of the rules'): review what it does, and add a rule or an override (catalogs/effects.go)", api, op)
}

// byRule is the effect the rules give an operation's name: the first verb it starts with, as a
// word ("TagResource" starts with Tag; "Tagged" would not).
func byRule(op string) (protocol.Effect, bool) {
	for _, r := range rules {
		for _, v := range r.verbs {
			rest, ok := strings.CutPrefix(op, v)
			if !ok {
				continue
			}
			if c, _ := utf8.DecodeRuneInString(rest); rest == "" || unicode.IsUpper(c) {
				return r.effect, true
			}
		}
	}
	return "", false
}
