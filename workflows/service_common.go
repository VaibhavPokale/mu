package workflows

import (
	"github.com/pkg/errors"
	"github.com/stelligent/mu/common"
	"github.com/stelligent/mu/templates"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

type serviceWorkflow struct {
	envStack          *common.Stack
	lbStack           *common.Stack
	artifactProvider  common.ArtifactProvider
	serviceName       string
	serviceTag        string
	serviceImage      string
	registryAuth      string
	priority          int
	codeRevision      string
	repoName          string
	appName           string
	appRevisionBucket string
	appRevisionKey    string
}

// Find a service in config, by name and set the reference
func (workflow *serviceWorkflow) serviceLoader(ctx *common.Context, tag string, provider string) Executor {
	return func() error {
		err := workflow.serviceInput(ctx, "")()
		if err != nil {
			return err
		}

		// Tag
		if tag != "" {
			workflow.serviceTag = tag
		} else if ctx.Config.Repo.Revision != "" {
			workflow.serviceTag = ctx.Config.Repo.Revision
		} else {
			workflow.serviceTag = "latest"
		}
		workflow.appRevisionKey = fmt.Sprintf("%s/%s.zip", workflow.serviceName, workflow.serviceTag)

		workflow.codeRevision = ctx.Config.Repo.Revision
		workflow.repoName = ctx.Config.Repo.Slug
		workflow.priority = ctx.Config.Service.Priority

		if provider == "" {
			dockerfile := ctx.Config.Service.Dockerfile
			if dockerfile == "" {
				dockerfile = "Dockerfile"
			}

			dockerfilePath := fmt.Sprintf("%s/%s", ctx.Config.Basedir, dockerfile)
			log.Debugf("Determining repo provider by checking for existence of '%s'", dockerfilePath)

			if _, err := os.Stat(dockerfilePath); !os.IsNotExist(err) {
				workflow.artifactProvider = common.ArtifactProviderEcr
			} else {
				workflow.artifactProvider = common.ArtifactProviderS3
			}
		} else {
			workflow.artifactProvider = common.ArtifactProvider(provider)
		}

		log.Debugf("Working with service:'%s' tag:'%s'", workflow.serviceName, workflow.serviceTag)
		return nil
	}
}

func (workflow *serviceWorkflow) isEcrProvider() Conditional {
	return func() bool {
		return strings.EqualFold(string(workflow.artifactProvider), string(common.ArtifactProviderEcr))
	}
}

func (workflow *serviceWorkflow) isS3Provider() Conditional {
	return func() bool {
		return strings.EqualFold(string(workflow.artifactProvider), string(common.ArtifactProviderS3))
	}
}

func (workflow *serviceWorkflow) isEcsProvider() Conditional {
	return func() bool {
		return strings.EqualFold(string(workflow.envStack.Tags["provider"]), string(common.EnvProviderEcs))
	}
}

func (workflow *serviceWorkflow) isEc2Provider() Conditional {
	return func() bool {
		return strings.EqualFold(string(workflow.envStack.Tags["provider"]), string(common.EnvProviderEc2))
	}
}

func (workflow *serviceWorkflow) serviceInput(ctx *common.Context, serviceName string) Executor {
	return func() error {
		// Repo Name
		if serviceName != "" {
			workflow.serviceName = serviceName
		} else if ctx.Config.Service.Name != "" {
			workflow.serviceName = ctx.Config.Service.Name
		} else if ctx.Config.Repo.Name != "" {
			workflow.serviceName = ctx.Config.Repo.Name
		} else {
			return errors.New("Service name must be provided")
		}
		return nil
	}
}

func (workflow *serviceWorkflow) serviceRepoUpserter(namespace string, service *common.Service, stackUpserter common.StackUpserter, stackWaiter common.StackWaiter) Executor {
	return func() error {
		if service.ImageRepository != "" {
			log.Noticef("Using repo '%s' for service '%s'", service.ImageRepository, workflow.serviceName)
			workflow.serviceImage = service.ImageRepository
			return nil
		}

		log.Noticef("Upsert repo for service '%s'", workflow.serviceName)

		ecrStackName := common.CreateStackName(namespace, common.StackTypeRepo, workflow.serviceName)
		overrides := common.GetStackOverrides(ecrStackName)
		template, err := templates.NewTemplate("repo.yml", nil, overrides)
		if err != nil {
			return err
		}

		stackParams := make(map[string]string)
		stackParams["RepoName"] = workflow.serviceName

		var envTags TagInterface = &EnvironmentTags{
			Environment: workflow.serviceName,
			Type: string(common.StackTypeRepo),
			Provider: "", 
			Revision: workflow.codeRevision,
			Repo: workflow.repoName,
		}
		tags, err := concatTags(service.Tags, envTags)
		if err != nil {
			return err
		}

		err = stackUpserter.UpsertStack(ecrStackName, template, stackParams, tags)
		if err != nil {
			return err
		}

		log.Debugf("Waiting for stack '%s' to complete", ecrStackName)
		stack := stackWaiter.AwaitFinalStatus(ecrStackName)
		if stack == nil {
			return fmt.Errorf("Unable to create stack %s", ecrStackName)
		}
		if strings.HasSuffix(stack.Status, "ROLLBACK_COMPLETE") || !strings.HasSuffix(stack.Status, "_COMPLETE") {
			return fmt.Errorf("Ended in failed status %s %s", stack.Status, stack.StatusReason)
		}
		workflow.serviceImage = fmt.Sprintf("%s:%s", stack.Outputs["RepoUrl"], workflow.serviceTag)
		return nil
	}
}
func (workflow *serviceWorkflow) serviceAppUpserter(namespace string, service *common.Service, stackUpserter common.StackUpserter, stackWaiter common.StackWaiter) Executor {
	return func() error {
		log.Noticef("Upsert app for service '%s'", workflow.serviceName)

		appStackName := common.CreateStackName(namespace, common.StackTypeApp, workflow.serviceName)
		overrides := common.GetStackOverrides(appStackName)
		template, err := templates.NewTemplate("app.yml", nil, overrides)
		if err != nil {
			return err
		}

		stackParams := make(map[string]string)

		var envTags TagInterface = &EnvironmentTags{
			Environment: workflow.serviceName,
			Type: string(common.StackTypeApp),
			Provider: "", 
			Revision: workflow.codeRevision,
			Repo: workflow.repoName,
		}

		tags, err := concatTags(service.Tags, envTags)
		if err != nil {
			return err
		}

		err = stackUpserter.UpsertStack(appStackName, template, stackParams, tags)
		if err != nil {
			return err
		}

		log.Debugf("Waiting for stack '%s' to complete", appStackName)
		stack := stackWaiter.AwaitFinalStatus(appStackName)
		if stack == nil {
			return fmt.Errorf("Unable to create stack %s", appStackName)
		}
		if strings.HasSuffix(stack.Status, "ROLLBACK_COMPLETE") || !strings.HasSuffix(stack.Status, "_COMPLETE") {
			return fmt.Errorf("Ended in failed status %s %s", stack.Status, stack.StatusReason)
		}
		workflow.appName = stack.Outputs["ApplicationName"]
		return nil
	}
}
func (workflow *serviceWorkflow) serviceBucketUpserter(namespace string, service *common.Service, stackUpserter common.StackUpserter, stackWaiter common.StackWaiter) Executor {
	return func() error {
		bucketStackName := common.CreateStackName(namespace, common.StackTypeBucket, "codedeploy")
		overrides := common.GetStackOverrides(bucketStackName)
		template, err := templates.NewTemplate("bucket.yml", nil, overrides)
		if err != nil {
			return err
		}
		log.Noticef("Upserting Bucket for CodeDeploy")
		bucketParams := make(map[string]string)
		bucketParams["BucketPrefix"] = "codedeploy"

		var pipeTags TagInterface = &PipelineTags{
			Type: common.StackTypeBucket,
			Service: workflow.serviceName,
			Revision: workflow.codeRevision,
			Repo: workflow.repoName,
		}
		tags, err := concatTags(service.Tags, pipeTags)
		if err != nil {
			return err
		}

		err = stackUpserter.UpsertStack(bucketStackName, template, bucketParams, tags)
		if err != nil {
			return err
		}

		log.Debugf("Waiting for stack '%s' to complete", bucketStackName)
		stack := stackWaiter.AwaitFinalStatus(bucketStackName)
		if stack == nil {
			return fmt.Errorf("Unable to create stack %s", bucketStackName)
		}
		if strings.HasSuffix(stack.Status, "ROLLBACK_COMPLETE") || !strings.HasSuffix(stack.Status, "_COMPLETE") {
			return fmt.Errorf("Ended in failed status %s %s", stack.Status, stack.StatusReason)
		}

		workflow.appRevisionBucket = stack.Outputs["Bucket"]

		return nil
	}
}

func (workflow *serviceWorkflow) serviceRegistryAuthenticator(authenticator common.RepositoryAuthenticator) Executor {
	return func() error {
		log.Debugf("Authenticating to registry '%s'", workflow.serviceImage)
		registryAuth, err := authenticator.AuthenticateRepository(workflow.serviceImage)
		if err != nil {
			return err
		}

		data, err := base64.StdEncoding.DecodeString(registryAuth)
		if err != nil {
			return err
		}

		authParts := strings.Split(string(data), ":")

		workflow.registryAuth = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("{\"username\":\"%s\", \"password\":\"%s\"}", authParts[0], authParts[1])))
		return nil
	}
}
