package e2e_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	routev1 "github.com/openshift/api/route/v1"
	ofapi "github.com/operator-framework/api/pkg/operators/v1alpha1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	k8sclient "k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntime "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	componentsv1 "github.com/opendatahub-io/opendatahub-operator/v2/apis/components/v1"
	dscv1 "github.com/opendatahub-io/opendatahub-operator/v2/apis/datasciencecluster/v1"
	dsciv1 "github.com/opendatahub-io/opendatahub-operator/v2/apis/dscinitialization/v1"
	featurev1 "github.com/opendatahub-io/opendatahub-operator/v2/apis/features/v1"
)

type TestFn func(t *testing.T)

var (
	testOpts testContextConfig
	scheme   = runtime.NewScheme()

	componentsTestSuites = map[string]TestFn{
		"dashboard":             dashboardTestSuite,
		"ray":                   rayTestSuite,
		"modelregistry":         modelRegistryTestSuite,
		"trustyai":              trustyAITestSuite,
		"kueue":                 kueueTestSuite,
		"trainingoperator":      trainingoperatorTestSuite,
		"datasciencepipelienes": dataSciencePipelinesTestSuite,
		"codeflare":             codeflareTestSuite,
	}
)

type arrayFlags []string

func (i *arrayFlags) String() string {
	return fmt.Sprintf("%v", *i)
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

type testContextConfig struct {
	operatorNamespace string
	skipDeletion      bool

	operatorControllerTest bool
	webhookTest            bool
	components             arrayFlags
}

// Holds information specific to individual tests.
type testContext struct {
	// Rest config
	cfg *rest.Config
	// client for k8s resources
	kubeClient *k8sclient.Clientset
	// custom client for managing custom resources
	customClient client.Client
	// namespace of the operator
	operatorNamespace string
	// namespace of the deployed applications
	applicationsNamespace string
	// test DataScienceCluster instance
	testDsc *dscv1.DataScienceCluster
	// test DSCI CR because we do not create it in ODH by default
	testDSCI *dsciv1.DSCInitialization
	// context for accessing resources
	//nolint:containedctx //reason: legacy v1 test setup
	ctx context.Context
	// test configuration
	testOpts testContextConfig
}

func NewTestContext() (*testContext, error) {
	// GetConfig(): If KUBECONFIG env variable is set, it is used to create
	// the client, else the inClusterConfig() is used.
	// Lastly if none of them are set, it uses  $HOME/.kube/config to create the client.
	config, err := ctrlruntime.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("error creating the config object %w", err)
	}

	kc, err := k8sclient.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Kubernetes client: %w", err)
	}

	// custom client to manages resources like Route etc
	custClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize custom client: %w", err)
	}

	// setup DSCI CR since we do not create automatically by operator
	testDSCI := setupDSCICR("e2e-test-dsci")
	// Setup DataScienceCluster CR
	testDSC := setupDSCInstance("e2e-test-dsc")

	return &testContext{
		cfg:                   config,
		kubeClient:            kc,
		customClient:          custClient,
		operatorNamespace:     testOpts.operatorNamespace,
		applicationsNamespace: testDSCI.Spec.ApplicationsNamespace,
		ctx:                   context.TODO(),
		testDsc:               testDSC,
		testDSCI:              testDSCI,
		testOpts:              testOpts,
	}, nil
}

// TestOdhOperator sets up the testing suite for ODH Operator.
func TestOdhOperator(t *testing.T) {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(routev1.AddToScheme(scheme))
	utilruntime.Must(apiextv1.AddToScheme(scheme))
	utilruntime.Must(autoscalingv1.AddToScheme(scheme))
	utilruntime.Must(dsciv1.AddToScheme(scheme))
	utilruntime.Must(dscv1.AddToScheme(scheme))
	utilruntime.Must(featurev1.AddToScheme(scheme))
	utilruntime.Must(monitoringv1.AddToScheme(scheme))
	utilruntime.Must(ofapi.AddToScheme(scheme))
	utilruntime.Must(operatorv1.AddToScheme(scheme))
	utilruntime.Must(componentsv1.AddToScheme(scheme))

	log.SetLogger(zap.New(zap.UseDevMode(true)))

	if testOpts.operatorControllerTest {
		// individual test suites after the operator is running
		if !t.Run("validate operator pod is running", testODHOperatorValidation) {
			return
		}
	}

	// Run create and delete tests for all the components
	t.Run("create DSCI and DSC CRs", creationTestSuite)

	for k, v := range componentsTestSuites {
		if len(testOpts.components) != 0 && !slices.Contains(testOpts.components, k) {
			t.Logf("Skipping tests for component %s", k)
			continue
		}

		t.Run("validate installation of "+k+" component", v)
	}

	// Run deletion if skipDeletion is not set
	if !testOpts.skipDeletion {
		if testOpts.operatorControllerTest {
			// this is a negative test case, since by using the positive CM('true'), even CSV gets deleted which leaves no operator pod in prow
			t.Run("components should not be removed if labeled is set to 'false' on configmap", cfgMapDeletionTestSuite)
		}

		t.Run("delete components", deletionTestSuite)
	}
}

func TestMain(m *testing.M) {
	// call flag.Parse() here if TestMain uses flags
	flag.StringVar(&testOpts.operatorNamespace, "operator-namespace", "opendatahub-operator-system", "Namespace where the odh operator is deployed")
	flag.BoolVar(&testOpts.skipDeletion, "skip-deletion", false, "skip deletion of the controllers")

	flag.BoolVar(&testOpts.operatorControllerTest, "test-operator-controller", true, "run operator controller tests")
	flag.BoolVar(&testOpts.webhookTest, "test-webhook", true, "run webhook tests")

	componentNames := strings.Join(maps.Keys(componentsTestSuites), ", ")
	flag.Var(&testOpts.components, "test-component", "run tests for the specified component. valid components names are: "+componentNames)

	flag.Parse()

	for _, n := range testOpts.components {
		if _, ok := componentsTestSuites[n]; !ok {
			fmt.Printf("test-component: unknown component %s, valid values are: %s", n, componentNames)
			os.Exit(1)
		}
	}

	os.Exit(m.Run())
}
