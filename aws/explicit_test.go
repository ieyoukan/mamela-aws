package aws_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela-aws/aws"
	"github.com/ieyoukan/mamela-aws/fakeaws"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// stateOf decodes an observed state.
func stateOf[T any](t *testing.T, o protocol.Observed) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(o.State, &v); err != nil {
		t.Fatalf("%s: %v", o.ID, err)
	}
	return v
}

// An imported stack, adopted object by object, is in sync with its declaration at once: nothing
// is planned for it before (it is to adopt, or waits for what it refers to), and nothing after.
func TestAdoptedStackIsInSync(t *testing.T) {
	api := fakeaws.New()
	st := api.AddTerraformStack("locust")
	res, err := aws.Provider.Import(context.Background(), protocol.ImportParams{Objects: importState(t, st.State), Settings: stackGuard})
	if err != nil {
		t.Fatal(err)
	}
	desired, adopting := declare(res)
	h := newHost(t, api)
	if cs := h.diff(desired, h.observe(desired, adopting)); len(cs) > 0 {
		t.Fatalf("before adopting, %s %s is planned", cs[0].Op, cs[0].Resource)
	}
	for _, id := range stackOrder {
		os := h.observe(desired, adopting)
		o := observed(t, os, id)
		if !o.Adoptable || o.Owned {
			t.Fatalf("%s is not adoptable: %+v", id, o)
		}
		d := &desired[slices.IndexFunc(desired, func(r protocol.Resource) bool { return r.ID == id })]
		c, err := h.prepare(aws.OpAdopt, d, o)
		if err != nil {
			t.Fatalf("adopt %s: %v", id, err)
		}
		if len(c.Requests) != 1 {
			t.Fatalf("adopt %s sends %d requests", id, len(c.Requests))
		}
		if err := h.apply(c, -1, ""); err != nil {
			t.Fatalf("adopt %s: %v", id, err)
		}
		if s := h.lookup(c); s != protocol.LookupDone {
			t.Fatalf("adopt %s: lookup says %s", id, s)
		}
		adopting = slices.DeleteFunc(adopting, func(a protocol.Adopting) bool { return a.Resource == id })
		// Adopting it again is refused: it carries Mamela's marks.
		if _, err := h.prepare(aws.OpAdopt, d, observed(t, h.observe(desired, adopting), id)); err == nil {
			t.Fatalf("%s is adopted twice", id)
		}
	}
	os := h.observe(desired, nil)
	for _, o := range os {
		if !o.Owned {
			t.Errorf("%s is not Mamela's after adopting it: %+v", o.ID, o)
		}
	}
	if cs := h.diff(desired, os); len(cs) > 0 {
		t.Fatalf("after adopting, %s %s is planned: %v", cs[0].Op, cs[0].Resource, cs[0].Reasons)
	}
	for _, s := range h.sent {
		if !slices.Contains([]string{"CreateTags", "UpdateRole", "Logs_20140328.TagResource", "AmazonEC2ContainerServiceV20141113.TagResource"}, s) {
			t.Errorf("adopting sent %s: it only marks", s)
		}
	}
	// The objects Terraform keeps are as they were: the role under /, the log group under /ecs/.
	if o := h.observe(nil, nil); slices.ContainsFunc(o, func(x protocol.Observed) bool {
		return x.Kind == aws.KindIAMRole && x.Name == "admin" || x.Kind == aws.KindLogGroup && x.Name == "old"
	}) {
		t.Error("an object Terraform keeps is Mamela's")
	}
}

// instanceWorld is an instance Mamela launched as t4g.micro; its declaration now says typ.
func instanceWorld(typ string) func(t *testing.T) ([]protocol.Resource, *host) {
	return func(t *testing.T) ([]protocol.Resource, *host) {
		api := fakeaws.New()
		if strings.HasSuffix(t.Name(), "as-it-should") {
			api.StopDelay = 1 // the instance is seen stopping once: the resize waits for it
		}
		h := newHost(t, api)
		desired := append(network(), res(aws.KindInstance, "target", `,"subnet":"public","securityGroup":"target","instanceType":"t4g.micro",`+
			`"container":{"image":"public.ecr.aws/nginx/nginx:stable","port":80}`, "AwsSubnet public", "AwsSecurityGroup target"))
		h.converge(desired, nil)
		i := len(desired) - 1
		desired[i].Spec = bytes.Replace(desired[i].Spec, []byte("t4g.micro"), []byte(typ), 1)
		return desired, h
	}
}

// A plan never changes an instance's type (that stops it): it shows the difference and how to
// resize; resize (stop, change the type, start) does it, and recovers from any of its requests
// not sent or answered.
func TestResize(t *testing.T) {
	desired, h := instanceWorld("t4g.small")(t)
	cs := h.diff(desired, h.observe(desired, nil))
	if len(cs) != 1 || cs[0].Op != aws.OpRetype || len(cs[0].Requests) > 0 || !strings.Contains(cs[0].Reasons[0], `mamela act --confirm target resize "AwsInstance target"`) {
		t.Fatalf("a declared type: %+v", cs)
	}
	explicitWithFaults(t, aws.OpResize, "AwsInstance target", instanceWorld("t4g.small"), func(t *testing.T, h *host) {
		h.converge(desired, nil) // a stopped one starts
		st := stateOf[struct {
			State        string `json:"state"`
			InstanceType string `json:"instanceType"`
		}](t, observed(t, h.observe(desired, nil), "AwsInstance target"))
		if st.InstanceType != "t4g.small" || st.State != "running" {
			t.Fatalf("after resize: %+v", st)
		}
	})
}

func TestReboot(t *testing.T) {
	explicitWithFaults(t, aws.OpReboot, "AwsInstance target", instanceWorld("t4g.micro"), func(t *testing.T, h *host) {
		st := stateOf[struct {
			State string `json:"state"`
		}](t, observed(t, h.observe(nil, nil), "AwsInstance target"))
		if st.State != "running" {
			t.Fatalf("after reboot: %+v", st)
		}
	})
}

// logWorld is a log group Mamela made, kept 30 days, that stores bytes of logs; its declaration
// now says days.
func logWorld(days string, stored int64) func(t *testing.T) ([]protocol.Resource, *host) {
	return func(t *testing.T) ([]protocol.Resource, *host) {
		api := fakeaws.New()
		h := newHost(t, api)
		desired := []protocol.Resource{res(aws.KindLogGroup, "app", `,"retentionDays":30`)}
		h.converge(desired, nil)
		api.SetLogBytes("/mamela/locust/app", stored)
		desired[0].Spec = bytes.Replace(desired[0].Spec, []byte("30"), []byte(days), 1)
		return desired, h
	}
}

// A plan keeps logs longer, and sets the retention of a log group that stores nothing; it never
// shortens the retention of one that stores logs (that deletes the older ones): it shows how to,
// and shorten-retention does it.
func TestRetention(t *testing.T) {
	for _, c := range []struct {
		days   string
		stored int64
		op     protocol.Op
	}{{"60", 1 << 20, aws.OpRetention}, {"7", 0, aws.OpRetention}, {"7", 1 << 20, aws.OpShorten}} {
		desired, h := logWorld(c.days, c.stored)(t)
		cs := h.diff(desired, h.observe(desired, nil))
		if len(cs) != 1 || cs[0].Op != c.op {
			t.Fatalf("%s days, %d bytes stored: %+v", c.days, c.stored, cs)
		}
		if c.op == aws.OpShorten && !strings.Contains(cs[0].Reasons[0], `mamela act --confirm app shorten-retention "AwsLogGroup app"`) {
			t.Errorf("the plan does not say how to shorten it: %v", cs[0].Reasons)
		}
	}
	desired, h := logWorld("60", 1<<20)(t)
	if _, err := h.prepare(aws.OpShortenRetention, &desired[0], observed(t, h.observe(desired, nil), "AwsLogGroup app")); err == nil {
		t.Error("shorten-retention lengthens it")
	}
	explicitWithFaults(t, aws.OpShortenRetention, "AwsLogGroup app", logWorld("7", 1<<20), func(t *testing.T, h *host) {
		desired := []protocol.Resource{res(aws.KindLogGroup, "app", `,"retentionDays":7`)}
		if cs := h.diff(desired, h.observe(desired, nil)); len(cs) > 0 {
			t.Fatalf("after shortening: %+v", cs)
		}
	})
}
