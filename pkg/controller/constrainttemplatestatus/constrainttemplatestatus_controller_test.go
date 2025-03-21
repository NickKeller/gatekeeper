package constrainttemplatestatus_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/open-policy-agent/frameworks/constraint/pkg/apis/templates/v1beta1"
	constraintclient "github.com/open-policy-agent/frameworks/constraint/pkg/client"
	"github.com/open-policy-agent/frameworks/constraint/pkg/client/drivers/rego"
	podstatus "github.com/open-policy-agent/gatekeeper/v3/apis/status/v1beta1"
	"github.com/open-policy-agent/gatekeeper/v3/pkg/controller/constrainttemplate"
	"github.com/open-policy-agent/gatekeeper/v3/pkg/fakes"
	"github.com/open-policy-agent/gatekeeper/v3/pkg/readiness"
	"github.com/open-policy-agent/gatekeeper/v3/pkg/target"
	"github.com/open-policy-agent/gatekeeper/v3/pkg/util"
	"github.com/open-policy-agent/gatekeeper/v3/pkg/watch"
	testclient "github.com/open-policy-agent/gatekeeper/v3/test/clients"
	"github.com/open-policy-agent/gatekeeper/v3/test/testutils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const (
	timeout = 15 * time.Second

	contraintTemplateName = "denyall"
	constraintCRDName     = "DenyAll"
)

// setupManager sets up a controller-runtime manager with registered watch manager.
func setupManager(t *testing.T) (manager.Manager, *watch.Manager) {
	t.Helper()

	mgr, err := manager.New(cfg, manager.Options{
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		MapperProvider: apiutil.NewDynamicRESTMapper,
		Logger:         testutils.NewLogger(t),
	})
	if err != nil {
		t.Fatalf("setting up controller manager: %s", err)
	}
	c := mgr.GetCache()
	dc, ok := c.(watch.RemovableCache)
	if !ok {
		t.Fatalf("expected dynamic cache, got: %T", c)
	}
	wm, err := watch.New(dc)
	if err != nil {
		t.Fatalf("could not create watch manager: %s", err)
	}
	if err := mgr.Add(wm); err != nil {
		t.Fatalf("could not add watch manager to manager: %s", err)
	}
	return mgr, wm
}

func TestReconcile(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	template := &v1beta1.ConstraintTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: contraintTemplateName},
		Spec: v1beta1.ConstraintTemplateSpec{
			CRD: v1beta1.CRD{
				Spec: v1beta1.CRDSpec{
					Names: v1beta1.Names{
						Kind: constraintCRDName,
					},
				},
			},
			Targets: []v1beta1.Target{
				{
					Target: target.Name,
					Rego: `
package foo

violation[{"msg": "denied!"}] {
	1 == 1
}
`,
				},
			},
		},
	}

	// Uncommenting the below enables logging of K8s internals like watch.
	// fs := flag.NewFlagSet("", flag.PanicOnError)
	// klog.InitFlags(fs)
	// fs.Parse([]string{"--alsologtostderr", "-v=10"})
	// klog.SetOutput(os.Stderr)

	// Setup the Manager and Controller.  Wrap the Controller Reconcile function so it writes each request to a
	// channel when it is finished.
	mgr, wm := setupManager(t)
	c := testclient.NewRetryClient(mgr.GetClient())

	// creating the gatekeeper-system namespace is necessary because that's where
	// status resources live by default
	if err := testutils.CreateGatekeeperNamespace(mgr.GetConfig()); err != nil {
		t.Fatalf("want createGatekeeperNamespace(mgr.GetConfig()) error = nil, got %v", err)
	}

	driver, err := rego.New(rego.Tracing(true))
	if err != nil {
		t.Fatalf("unable to set up Driver: %v", err)
	}

	cfClient, err := constraintclient.NewClient(constraintclient.Targets(&target.K8sValidationTarget{}), constraintclient.Driver(driver), constraintclient.EnforcementPoints(util.AuditEnforcementPoint))
	if err != nil {
		t.Fatalf("unable to set up constraint framework client: %s", err)
	}

	testutils.Setenv(t, "POD_NAME", "no-pod")

	tracker, err := readiness.SetupTracker(mgr, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	pod := fakes.Pod(
		fakes.WithNamespace("gatekeeper-system"),
		fakes.WithName("no-pod"),
	)

	adder := constrainttemplate.Adder{
		CFClient:     cfClient,
		WatchManager: wm,
		Tracker:      tracker,
		GetPod:       func(context.Context) (*corev1.Pod, error) { return pod, nil },
	}
	err = adder.Add(mgr)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	testutils.StartManager(ctx, t, mgr)

	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	t.Cleanup(cancel)
	mgr.GetCache().WaitForCacheSync(waitCtx)

	templateCpy := template.DeepCopy()
	t.Run("Constraint template status gets created and reported", func(t *testing.T) {
		g.Eventually(verifyTStatusCount(ctx, c, 0), timeout).Should(gomega.BeNil())
		err := c.Create(ctx, templateCpy)
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyTStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
		g.Eventually(verifyTStatusCreated(ctx, c, true), timeout).Should(gomega.BeNil())
		g.Eventually(verifyTByPodStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
	})

	constraint := newDenyAllConstraint()
	t.Run("Constraint status gets created and reported", func(t *testing.T) {
		g.Eventually(verifyCStatusCount(ctx, c, 0), timeout).Should(gomega.BeNil())
		err := c.Create(ctx, constraint)
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyCStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
		g.Eventually(verifyCByPodStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
	})

	fakePod := pod.DeepCopy()
	fakePod.SetName("fake-pod")
	t.Run("Multiple constraint template statuses are reported", func(t *testing.T) {
		fakeTStatus, err := podstatus.NewConstraintTemplateStatusForPod(fakePod, contraintTemplateName, mgr.GetScheme())
		if err != nil {
			t.Fatal(err)
		}
		fakeTStatus.Status.TemplateUID = templateCpy.UID

		// TODO: Test if this removal is necessary.
		// https://github.com/open-policy-agent/gatekeeper/pull/1595#discussion_r722819552
		t.Cleanup(testutils.DeleteObject(t, c, fakeTStatus))

		err = c.Create(ctx, fakeTStatus)
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyTStatusCreated(ctx, c, true), timeout).Should(gomega.BeNil())
		g.Eventually(verifyTByPodStatusCount(ctx, c, 2), timeout).Should(gomega.BeNil())
		err = c.Delete(ctx, fakeTStatus)
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyTByPodStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
	})

	t.Run("Constraint template status.created is true even if some pod has errors", func(t *testing.T) {
		fakeTStatus, err := podstatus.NewConstraintTemplateStatusForPod(fakePod, contraintTemplateName, mgr.GetScheme())
		if err != nil {
			t.Fatal(err)
		}
		fakeTStatus.Status.TemplateUID = templateCpy.UID
		fakeTStatus.Status.Errors = []*v1beta1.CreateCRDError{{
			Code:    constrainttemplate.ErrCreateCode,
			Message: "Could not create CRD: error",
		}}

		// TODO: Test if this removal is necessary.
		// https://github.com/open-policy-agent/gatekeeper/pull/1595#discussion_r722819552
		t.Cleanup(testutils.DeleteObject(t, c, fakeTStatus))

		err = c.Create(ctx, fakeTStatus)
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyTStatusCreated(ctx, c, true), timeout).Should(gomega.BeNil())
		g.Eventually(verifyTByPodStatusCount(ctx, c, 2), timeout).Should(gomega.BeNil())
		err = c.Delete(ctx, fakeTStatus)
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyTByPodStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
	})

	t.Run("Multiple constraint statuses are reported", func(t *testing.T) {
		fakeCStatus, err := podstatus.NewConstraintStatusForPod(fakePod, newDenyAllConstraint(), mgr.GetScheme())
		if err != nil {
			t.Fatal(err)
		}
		fakeCStatus.Status.ConstraintUID = constraint.GetUID()
		if err != nil {
			t.Fatal(err)
		}
		err = c.Create(ctx, fakeCStatus)
		if err != nil {
			t.Fatal(err)
		}

		// TODO: Test if this removal is necessary.
		// https://github.com/open-policy-agent/gatekeeper/pull/1595#discussion_r722819552
		t.Cleanup(testutils.DeleteObject(t, c, fakeCStatus))

		g.Eventually(verifyCByPodStatusCount(ctx, c, 2), timeout).Should(gomega.BeNil())
		err = c.Delete(ctx, fakeCStatus)
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyCByPodStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
	})

	t.Run("Deleting a constraint deletes its status", func(t *testing.T) {
		err := c.Delete(ctx, constraint)
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyCStatusCount(ctx, c, 0), timeout).Should(gomega.BeNil())
		constraint = newDenyAllConstraint()
		err = c.Create(ctx, constraint)
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyCStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
		g.Eventually(verifyCByPodStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
	})

	t.Run("Deleting a constraint template deletes all statuses", func(t *testing.T) {
		err := c.Delete(ctx, template.DeepCopy())
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyTStatusCount(ctx, c, 0), timeout).Should(gomega.BeNil())
		g.Eventually(verifyCStatusCount(ctx, c, 0), timeout).Should(gomega.BeNil())
		// need to manually delete constraint currently as garbage collection does not run on the test API server
		err = c.Delete(ctx, newDenyAllConstraint())
		if err != nil {
			t.Fatal(err)
		}
	})

	templateCpy = template.DeepCopy()
	constraint = newDenyAllConstraint()
	t.Run("Deleting a constraint template deletes all statuses for the current pod", func(t *testing.T) {
		g.Eventually(verifyTStatusCount(ctx, c, 0), timeout).Should(gomega.BeNil())
		err := c.Create(ctx, templateCpy)
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyTStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
		g.Eventually(verifyTStatusCreated(ctx, c, true), timeout).Should(gomega.BeNil())
		g.Eventually(verifyTByPodStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
		g.Eventually(verifyCStatusCount(ctx, c, 0), timeout).Should(gomega.BeNil())
		err = c.Create(ctx, constraint)
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyCStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
		g.Eventually(verifyCByPodStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())

		fakeTStatus, err := podstatus.NewConstraintTemplateStatusForPod(fakePod, contraintTemplateName, mgr.GetScheme())
		if err != nil {
			t.Fatal(err)
		}
		fakeTStatus.Status.TemplateUID = templateCpy.UID
		err = c.Create(ctx, fakeTStatus)
		if err != nil {
			t.Fatal(err)
		}

		// TODO: Test if this removal is necessary.
		// https://github.com/open-policy-agent/gatekeeper/pull/1595#discussion_r722819552
		t.Cleanup(testutils.DeleteObject(t, c, fakeTStatus))

		fakeCStatus, err := podstatus.NewConstraintStatusForPod(fakePod, newDenyAllConstraint(), mgr.GetScheme())
		if err != nil {
			t.Fatal(err)
		}
		fakeCStatus.Status.ConstraintUID = constraint.GetUID()
		err = c.Create(ctx, fakeCStatus)
		if err != nil {
			t.Fatal(err)
		}

		// TODO: Test if this removal is necessary.
		// https://github.com/open-policy-agent/gatekeeper/pull/1595#discussion_r722819552
		t.Cleanup(testutils.DeleteObject(t, c, fakeCStatus))

		g.Eventually(verifyTByPodStatusCount(ctx, c, 2), 30*timeout).Should(gomega.BeNil())
		g.Eventually(verifyCByPodStatusCount(ctx, c, 2), timeout).Should(gomega.BeNil())
		err = c.Delete(ctx, template.DeepCopy())
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyTStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
		g.Eventually(verifyCStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
	})

	templateCpy = template.DeepCopy()
	templateCpy.Spec.Targets[0].Rego = `
	package foo

	violation[invalid syntax error`

	t.Run("Invalid ContraintTemplate has .status.created set to false", func(t *testing.T) {
		g.Eventually(verifyTStatusCount(ctx, c, 0), timeout).Should(gomega.BeNil())
		err := c.Create(ctx, templateCpy)
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyTStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
		g.Eventually(verifyTByPodStatusCount(ctx, c, 1), timeout).Should(gomega.BeNil())
		g.Eventually(verifyTStatusCreated(ctx, c, false), timeout).Should(gomega.BeNil())

		err = c.Delete(ctx, templateCpy)
		if err != nil {
			t.Fatal(err)
		}
		g.Eventually(verifyTStatusCount(ctx, c, 0), timeout).Should(gomega.BeNil())
	})
}

func verifyTStatusCount(ctx context.Context, c client.Client, expected int) func() error {
	return func() error {
		statuses := &podstatus.ConstraintTemplatePodStatusList{}
		if err := c.List(ctx, statuses, client.InNamespace("gatekeeper-system")); err != nil {
			return err
		}
		if len(statuses.Items) != expected {
			return fmt.Errorf("status count = %d, wanted %d, statuses: %+v", len(statuses.Items), expected, statuses.Items)
		}
		return nil
	}
}

func verifyTStatusCreated(ctx context.Context, c client.Client, expected bool) func() error {
	return func() error {
		ct := &v1beta1.ConstraintTemplate{}
		if err := c.Get(ctx, types.NamespacedName{Name: contraintTemplateName}, ct); err != nil {
			return err
		}
		if ct.Status.Created != expected {
			return fmt.Errorf("status.created = %t, wanted %t", ct.Status.Created, expected)
		}
		return nil
	}
}

func verifyTByPodStatusCount(ctx context.Context, c client.Client, expected int) func() error {
	return func() error {
		ct := &v1beta1.ConstraintTemplate{}
		if err := c.Get(ctx, types.NamespacedName{Name: contraintTemplateName}, ct); err != nil {
			return err
		}
		if len(ct.Status.ByPod) != expected {
			return fmt.Errorf("status count = %d, wanted %d", len(ct.Status.ByPod), expected)
		}
		return nil
	}
}

func verifyCStatusCount(ctx context.Context, c client.Client, expected int) func() error {
	return func() error {
		statuses := &podstatus.ConstraintPodStatusList{}
		if err := c.List(ctx, statuses, client.InNamespace("gatekeeper-system")); err != nil {
			return err
		}
		if len(statuses.Items) != expected {
			return fmt.Errorf("status count = %d, wanted %d, statuses: %+v", len(statuses.Items), expected, statuses.Items)
		}
		return nil
	}
}

func verifyCByPodStatusCount(ctx context.Context, c client.Client, expected int) func() error {
	return func() error {
		constraint := newDenyAllConstraint()
		if err := c.Get(ctx, types.NamespacedName{Name: "denyallconstraint"}, constraint); err != nil {
			return err
		}
		statuses, _, err := unstructured.NestedSlice(constraint.Object, "status", "byPod")
		if err != nil {
			return err
		}
		if len(statuses) != expected {
			return fmt.Errorf("status count = %d, wanted %d", len(statuses), expected)
		}
		return nil
	}
}

func newDenyAllConstraint() *unstructured.Unstructured {
	constraint := &unstructured.Unstructured{}
	constraint.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "constraints.gatekeeper.sh",
		Version: "v1beta1",
		Kind:    constraintCRDName,
	})
	constraint.SetName(contraintTemplateName + "constraint")
	return constraint
}
