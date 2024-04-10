package scanner

import (
	"context"
	"fmt"
	"io/fs"

	"golang.org/x/xerrors"

	aws "github.com/aquasecurity/trivy-aws/pkg/scanner"
	"github.com/aquasecurity/trivy/pkg/cloud/aws/cache"
	"github.com/aquasecurity/trivy/pkg/commands/operation"
	"github.com/aquasecurity/trivy/pkg/flag"
	"github.com/aquasecurity/trivy/pkg/iac/framework"
	"github.com/aquasecurity/trivy/pkg/iac/scan"
	"github.com/aquasecurity/trivy/pkg/iac/scanners/options"
	"github.com/aquasecurity/trivy/pkg/iac/state"
	"github.com/aquasecurity/trivy/pkg/log"
	"github.com/aquasecurity/trivy/pkg/misconf"
)

type AWSScanner struct {
}

func NewScanner() *AWSScanner {
	return &AWSScanner{}
}

func (s *AWSScanner) Scan(ctx context.Context, option flag.Options) (scan.Results, bool, error) {

	awsCache := cache.New(option.CacheDir, option.MaxCacheAge, option.Account, option.Region)
	included, missing := awsCache.ListServices(option.Services)

	prefixedLogger := &log.PrefixedLogger{Name: "aws"}

	var scannerOpts []options.ScannerOption
	if !option.NoProgress {
		tracker := newProgressTracker(prefixedLogger)
		defer tracker.Finish()
		scannerOpts = append(scannerOpts, aws.ScannerWithProgressTracker(tracker))
	}

	if len(missing) > 0 {
		scannerOpts = append(scannerOpts, aws.ScannerWithAWSServices(missing...))
	}

	if option.Debug {
		scannerOpts = append(scannerOpts, options.ScannerWithDebug(prefixedLogger))
	}

	if option.Trace {
		scannerOpts = append(scannerOpts, options.ScannerWithTrace(prefixedLogger))
	}

	if option.Region != "" {
		scannerOpts = append(
			scannerOpts,
			aws.ScannerWithAWSRegion(option.Region),
		)
	}

	if option.Endpoint != "" {
		scannerOpts = append(
			scannerOpts,
			aws.ScannerWithAWSEndpoint(option.Endpoint),
		)
	}

	var policyPaths []string
	var downloadedPolicyPaths []string
	var err error

	var bundleRepo string
	switch {
	case len(option.MisconfOptions.PolicyBundleRepository) > 0:
		bundleRepo = option.MisconfOptions.PolicyBundleRepository
	case len(option.MisconfOptions.ChecksBundleRepository) > 0:
		bundleRepo = option.MisconfOptions.ChecksBundleRepository
	}

	downloadedPolicyPaths, err = operation.InitBuiltinPolicies(context.Background(), option.CacheDir, option.Quiet, option.SkipPolicyUpdate, bundleRepo, option.RegistryOpts())
	if err != nil {
		if !option.SkipPolicyUpdate {
			log.Logger.Errorf("Falling back to embedded policies: %s", err)
		}
	} else {
		log.Logger.Debug("Policies successfully loaded from disk")
		policyPaths = append(policyPaths, downloadedPolicyPaths...)
		scannerOpts = append(scannerOpts,
			options.ScannerWithEmbeddedPolicies(false),
			options.ScannerWithEmbeddedLibraries(false))
	}

	var policyFS fs.FS
	policyFS, policyPaths, err = misconf.CreatePolicyFS(append(policyPaths, option.RegoOptions.PolicyPaths...))
	if err != nil {
		return nil, false, xerrors.Errorf("unable to create policyfs: %w", err)
	}

	scannerOpts = append(scannerOpts,
		options.ScannerWithPolicyFilesystem(policyFS),
		options.ScannerWithPolicyDirs(policyPaths...),
	)

	dataFS, dataPaths, err := misconf.CreateDataFS(option.RegoOptions.DataPaths)
	if err != nil {
		log.Logger.Errorf("Could not load config data: %s", err)
	}
	scannerOpts = append(scannerOpts,
		options.ScannerWithDataDirs(dataPaths...),
		options.ScannerWithDataFilesystem(dataFS),
	)

	scannerOpts = addPolicyNamespaces(option.RegoOptions.PolicyNamespaces, scannerOpts)

	if option.Compliance.Spec.ID != "" {
		scannerOpts = append(scannerOpts, options.ScannerWithSpec(option.Compliance.Spec.ID))
	} else {
		scannerOpts = append(scannerOpts, options.ScannerWithFrameworks(
			framework.Default,
			framework.CIS_AWS_1_2))
	}

	scanner := aws.New(scannerOpts...)

	var freshState *state.State
	if len(missing) > 0 || option.CloudOptions.UpdateCache {
		var err error
		freshState, err = scanner.CreateState(ctx)
		if err != nil {
			return nil, false, err
		}
	}

	fullState, err := createState(freshState, awsCache)
	if err != nil {
		return nil, false, err
	}

	if fullState == nil {
		return nil, false, fmt.Errorf("no resultant state found")
	}

	if err := awsCache.AddServices(fullState, missing); err != nil {
		return nil, false, err
	}

	defsecResults, err := scanner.Scan(ctx, fullState)
	if err != nil {
		return nil, false, err
	}

	return defsecResults, len(included) > 0, nil
}

func createState(freshState *state.State, awsCache *cache.Cache) (*state.State, error) {
	var fullState *state.State
	if previousState, err := awsCache.LoadState(); err == nil {
		if freshState != nil {
			fullState, err = previousState.Merge(freshState)
			if err != nil {
				return nil, err
			}
		} else {
			fullState = previousState
		}
	} else {
		fullState = freshState
	}
	return fullState, nil
}

func addPolicyNamespaces(namespaces []string, scannerOpts []options.ScannerOption) []options.ScannerOption {
	if len(namespaces) > 0 {
		scannerOpts = append(
			scannerOpts,
			options.ScannerWithPolicyNamespaces(namespaces...),
		)
	}
	return scannerOpts
}
