// Commands for FlytePropeller controller.
package cmd

import (
	"context"
	"flag"
	"os"
	"runtime"
	"runtime/pprof"

	"github.com/flyteorg/flytestdlib/contextutils"

	transformers "github.com/flyteorg/flytepropeller/pkg/compiler/transformers/k8s"
	"github.com/flyteorg/flytepropeller/pkg/controller/executors"
	"k8s.io/klog"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	config2 "github.com/flyteorg/flytepropeller/pkg/controller/config"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/flyteorg/flytestdlib/config/viper"
	"github.com/flyteorg/flytestdlib/version"

	"github.com/flyteorg/flytestdlib/config"
	"github.com/flyteorg/flytestdlib/logger"
	"github.com/flyteorg/flytestdlib/profutils"
	"github.com/flyteorg/flytestdlib/promutils"
	"github.com/spf13/pflag"

	"github.com/spf13/cobra"

	clientset "github.com/flyteorg/flytepropeller/pkg/client/clientset/versioned"
	informers "github.com/flyteorg/flytepropeller/pkg/client/informers/externalversions"
	"github.com/flyteorg/flytepropeller/pkg/controller"
	"github.com/flyteorg/flytepropeller/pkg/signals"
	"github.com/flyteorg/flytepropeller/pkg/utils"
)

const (
	defaultNamespace = "all"
	appName          = "flytepropeller"
)

var (
	cfgFile        string
	configAccessor = viper.NewAccessor(config.Options{StrictMode: true})
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "flyte-propeller",
	Short: "Operator for running Flyte Workflows",
	Long: `Flyte Propeller runs a workflow to completion by recursing through the nodes, 
			handling their tasks to completion and propagating their status upstream.`,
	PersistentPreRunE: initConfig,
	Run: func(cmd *cobra.Command, args []string) {
		executeRootCmd(config2.GetConfig())
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	version.LogBuildInformation(appName)
	logger.Infof(context.TODO(), "Detected: %d CPU's\n", runtime.NumCPU())
	if err := rootCmd.Execute(); err != nil {
		logger.Error(context.TODO(), err)
		os.Exit(1)
	}
}

func init() {
	// allows `$ flytepropeller --logtostderr` to work
	klog.InitFlags(flag.CommandLine)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	err := flag.CommandLine.Parse([]string{})
	if err != nil {
		logAndExit(err)
	}

	// Here you will define your flags and configuration settings. Cobra supports persistent flags, which, if defined
	// here, will be global for your application.
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "",
		"config file (default is $HOME/config.yaml)")

	configAccessor.InitializePflags(rootCmd.PersistentFlags())

	rootCmd.AddCommand(viper.GetConfigCommand())
}

func initConfig(cmd *cobra.Command, _ []string) error {
	configAccessor = viper.NewAccessor(config.Options{
		StrictMode:  false,
		SearchPaths: []string{cfgFile},
	})

	configAccessor.InitializePflags(cmd.PersistentFlags())

	err := configAccessor.UpdateConfig(context.TODO())
	if err != nil {
		return err
	}

	return nil
}

func logAndExit(err error) {
	logger.Error(context.Background(), err)
	os.Exit(-1)
}

func sharedInformerOptions(cfg *config2.Config) []informers.SharedInformerOption {
	selectors := []struct {
		label     string
		operation v1.LabelSelectorOperator
		values    []string
	}{
		{transformers.ShardKeyLabel, v1.LabelSelectorOpIn, cfg.IncludeShardKeyLabel},
		{transformers.ShardKeyLabel, v1.LabelSelectorOpNotIn, cfg.ExcludeShardKeyLabel},
		{transformers.ProjectLabel, v1.LabelSelectorOpIn, cfg.IncludeProjectLabel},
		{transformers.ProjectLabel, v1.LabelSelectorOpNotIn, cfg.ExcludeProjectLabel},
		{transformers.DomainLabel, v1.LabelSelectorOpIn, cfg.IncludeDomainLabel},
		{transformers.DomainLabel, v1.LabelSelectorOpNotIn, cfg.ExcludeDomainLabel},
	}

	labelSelector := controller.IgnoreCompletedWorkflowsLabelSelector()
	for _, selector := range selectors {
		if len(selector.values) > 0 {
			labelSelectorRequirement := v1.LabelSelectorRequirement{
				Key:      selector.label,
				Operator: selector.operation,
				Values:   selector.values,
			}

			labelSelector.MatchExpressions = append(labelSelector.MatchExpressions, labelSelectorRequirement)
		}
	}

	opts := []informers.SharedInformerOption{
		informers.WithTweakListOptions(func(options *v1.ListOptions) {
			options.LabelSelector = v1.FormatLabelSelector(labelSelector)
		}),
	}

	if cfg.LimitNamespace != defaultNamespace {
		opts = append(opts, informers.WithNamespace(cfg.LimitNamespace))
	}
	return opts
}

func executeRootCmd(cfg *config2.Config) {
	baseCtx := context.Background()

	// set up signals so we handle the first shutdown signal gracefully
	ctx := signals.SetupSignalHandler(baseCtx)

	kubeClient, kubecfg, err := utils.GetKubeConfig(ctx, cfg)
	if err != nil {
		logger.Fatalf(ctx, "Error building kubernetes clientset: %s", err.Error())
	}

	flyteworkflowClient, err := clientset.NewForConfig(kubecfg)
	if err != nil {
		logger.Fatalf(ctx, "Error building example clientset: %s", err.Error())
	}

	opts := sharedInformerOptions(cfg)
	flyteworkflowInformerFactory := informers.NewSharedInformerFactoryWithOptions(flyteworkflowClient, cfg.WorkflowReEval.Duration, opts...)

	// Add the propeller subscope because the MetricsPrefix only has "flyte:" to get uniform collection of metrics.
	propellerScope := promutils.NewScope(cfg.MetricsPrefix).NewSubScope("propeller").NewSubScope(cfg.LimitNamespace)

	go func() {
		err := profutils.StartProfilingServerWithDefaultHandlers(ctx, cfg.ProfilerPort.Port, nil)
		if err != nil {
			logger.Panicf(ctx, "Failed to Start profiling and metrics server. Error: %v", err)
		}
	}()

	limitNamespace := ""
	if cfg.LimitNamespace != defaultNamespace {
		limitNamespace = cfg.LimitNamespace
	}

	mgr, err := manager.New(kubecfg, manager.Options{
		Namespace:     limitNamespace,
		SyncPeriod:    &cfg.DownstreamEval.Duration,
		ClientBuilder: executors.NewFallbackClientBuilder(propellerScope.NewSubScope("kube")),
	})
	if err != nil {
		logger.Fatalf(ctx, "Failed to initialize controller run-time manager. Error: %v", err)
	}

	// Start controller runtime manager to start listening to resource changes.
	// K8sPluginManager uses controller runtime to create informers for the CRDs being monitored by plugins. The informer
	// EventHandler enqueues the owner workflow for reevaluation. These informer events allow propeller to detect
	// workflow changes faster than the default sync interval for workflow CRDs.
	go func(ctx context.Context) {
		ctx = contextutils.WithGoroutineLabel(ctx, "controller-runtime-manager")
		pprof.SetGoroutineLabels(ctx)
		logger.Infof(ctx, "Starting controller-runtime manager")
		err := mgr.Start(ctx)
		if err != nil {
			logger.Fatalf(ctx, "Failed to start manager. Error: %v", err)
		}
	}(ctx)

	c, err := controller.New(ctx, cfg, kubeClient, flyteworkflowClient, flyteworkflowInformerFactory, mgr, propellerScope)
	if err != nil {
		logger.Fatalf(ctx, "Failed to start Controller - [%v]", err.Error())
		return
	} else if c == nil {
		logger.Fatalf(ctx, "Failed to start Controller, nil controller received.")
	}

	go flyteworkflowInformerFactory.Start(ctx.Done())

	if err = c.Run(ctx); err != nil {
		logger.Fatalf(ctx, "Error running controller: %s", err.Error())
	}
}
