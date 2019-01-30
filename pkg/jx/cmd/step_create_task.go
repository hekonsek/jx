package cmd

import (
	"fmt"
	"github.com/ghodss/yaml"
	"github.com/jenkins-x/jx/pkg/config"
	"github.com/jenkins-x/jx/pkg/jenkinsfile"
	"github.com/jenkins-x/jx/pkg/jenkinsfile/gitresolver"
	"github.com/jenkins-x/jx/pkg/jx/cmd/templates"
	"github.com/jenkins-x/jx/pkg/kube"
	"github.com/jenkins-x/jx/pkg/log"
	"github.com/jenkins-x/jx/pkg/util"
	pipelineapi "github.com/knative/build-pipeline/pkg/apis/pipeline/v1alpha1"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"gopkg.in/AlecAivazis/survey.v1/terminal"
	"io"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"path/filepath"
	"strings"
)

var (
	createTaskLong = templates.LongDesc(`
		Creates a Knative Pipeline Task for a project
`)

	createTaskExample = templates.Examples(`
		# create a Knative Pipeline Task and render to the console
		jx step create task

		# create a Knative Pipeline Task
		jx step create task -o mytask.yaml

			`)
)

// StepCreateTaskOptions contains the command line flags
type StepCreateTaskOptions struct {
	StepOptions

	Pack         string
	Dir          string
	OutputFile   string
	BuildPackURL string
	BuildPackRef string
	PipelineKind string

	PodTemplates        map[string]*corev1.Pod
	MissingPodTemplates map[string]bool
}

// NewCmdStepCreateTask Creates a new Command object
func NewCmdStepCreateTask(f Factory, in terminal.FileReader, out terminal.FileWriter, errOut io.Writer) *cobra.Command {
	options := &StepCreateTaskOptions{
		StepOptions: StepOptions{
			CommonOptions: CommonOptions{
				Factory: f,
				In:      in,
				Out:     out,
				Err:     errOut,
			},
		},
	}

	cmd := &cobra.Command{
		Use:     "task",
		Short:   "Creates a Knative Pipeline Task for the current folder or given build pack",
		Long:    createTaskLong,
		Example: createTaskExample,
		Aliases: []string{"bt"},
		Run: func(cmd *cobra.Command, args []string) {
			options.Cmd = cmd
			options.Args = args
			err := options.Run()
			CheckErr(err)
		},
	}
	options.addCommonFlags(cmd)

	cmd.Flags().StringVarP(&options.Dir, "dir", "d", "", "The directory to query to find the projects .git directory")
	cmd.Flags().StringVarP(&options.OutputFile, "output", "o", "", "The output file to write the output to as YAML")
	cmd.Flags().StringVarP(&options.BuildPackURL, "url", "u", "", "The URL for the build pack Git repository")
	cmd.Flags().StringVarP(&options.BuildPackRef, "ref", "r", "", "The Git reference (branch,tag,sha) in the Git repository to use")
	cmd.Flags().StringVarP(&options.Pack, "pack", "p", "", "The build pack name. If none is specified its discovered from the source code")
	cmd.Flags().StringVarP(&options.PipelineKind, "kind", "k", "release", "The kind of pipeline to create such as: "+strings.Join(jenkinsfile.PipelineKinds, ", "))
	return cmd
}

// Run implements this command
func (o *StepCreateTaskOptions) Run() error {
	settings, err := o.TeamSettings()
	if err != nil {
		return err
	}
	if o.BuildPackURL == "" || o.BuildPackRef == "" {
		if o.BuildPackURL == "" {
			o.BuildPackURL = settings.BuildPackURL
		}
		if o.BuildPackRef == "" {
			o.BuildPackRef = settings.BuildPackRef
		}
	}
	if o.BuildPackURL == "" {
		return util.MissingOption("url")
	}
	if o.BuildPackRef == "" {
		return util.MissingOption("ref")
	}
	if o.PipelineKind == "" {
		return util.MissingOption("kind")
	}
	if o.Dir == "" {
		o.Dir, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	projectConfig, projectConfigFile, err := config.LoadProjectConfig(o.Dir)
	if err != nil {
		return errors.Wrapf(err, "failed to load project config in dir %s", o.Dir)
	}
	if o.Pack == "" {
		o.Pack = projectConfig.BuildPack
	}
	if o.Pack == "" {
		o.Pack, err = o.discoverBuildPack(o.Dir, projectConfig)
	}

	if o.Pack == "" {
		return util.MissingOption("pack")
	}

	err = o.loadPodTemplates()
	if err != nil {
		return err
	}
	o.MissingPodTemplates = map[string]bool{}

	packsDir, err := gitresolver.InitBuildPack(o.Git(), o.BuildPackURL, o.BuildPackRef)
	if err != nil {
		return err
	}

	resolver, err := gitresolver.CreateResolver(packsDir, o.Git())
	if err != nil {
		return err
	}

	name := o.Pack
	packDir := filepath.Join(packsDir, name)

	pipelineFile := filepath.Join(packDir, jenkinsfile.PipelineConfigFileName)
	exists, err := util.FileExists(pipelineFile)
	if err != nil {
		return errors.Wrapf(err, "failed to find build pack pipeline YAML: %s", pipelineFile)
	}
	if !exists {
		return fmt.Errorf("no build pack for %s exists at directory %s", name, packDir)
	}
	jenkinsfileRunner := true
	pipelineConfig, err := jenkinsfile.LoadPipelineConfig(pipelineFile, resolver, jenkinsfileRunner)
	if err != nil {
		return errors.Wrapf(err, "failed to load build pack pipeline YAML: %s", pipelineFile)
	}
	localPipelineConfig := projectConfig.PipelineConfig
	if localPipelineConfig != nil {
		err = localPipelineConfig.ExtendPipeline(pipelineConfig, jenkinsfileRunner)
		if err != nil {
			return errors.Wrapf(err, "failed to override PipelineConfig using configuration in file %s", projectConfigFile)
		}
		pipelineConfig = localPipelineConfig
	}
	err = o.generateTask(name, pipelineConfig)
	if err != nil {
		return errors.Wrapf(err, "failed to generate Task for build pack pipeline YAML: %s", pipelineFile)
	}
	return err
}

func (o *StepCreateTaskOptions) loadPodTemplates() error {
	o.PodTemplates = map[string]*corev1.Pod{}

	kubeClient, ns, err := o.KubeClientAndDevNamespace()
	if err != nil {
		return err
	}
	configMapName := kube.ConfigMapJenkinsPodTemplates
	cm, err := kubeClient.CoreV1().ConfigMaps(ns).Get(configMapName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for k, v := range cm.Data {
		pod := &corev1.Pod{}
		if v != "" {
			err := yaml.Unmarshal([]byte(v), pod)
			if err != nil {
				return err
			}
			o.PodTemplates[k] = pod
		}
	}
	return nil
}

func (o *StepCreateTaskOptions) generateTask(name string, pipelineConfig *jenkinsfile.PipelineConfig) error {
	var lifecycles *jenkinsfile.PipelineLifecycles
	kind := o.PipelineKind
	pipelines := pipelineConfig.Pipelines
	switch kind {
	case jenkinsfile.PipelineKindRelease:
		lifecycles = pipelines.Release
	case jenkinsfile.PipelineKindPullRequest:
		lifecycles = pipelines.PullRequest
	case jenkinsfile.PipelineKindFeature:
		lifecycles = pipelines.Feature
	default:
		return fmt.Errorf("Unknown pipeline kind %s. Supported values are %s", kind, strings.Join(jenkinsfile.PipelineKinds, ", "))
	}
	return o.generatePipeline(name, pipelineConfig, lifecycles, kind)
}

func (o *StepCreateTaskOptions) generatePipeline(languageName string, pipelineConfig *jenkinsfile.PipelineConfig, lifecycles *jenkinsfile.PipelineLifecycles, templateKind string) error {
	if lifecycles == nil {
		return nil
	}

	container := pipelineConfig.Agent.Container
	dir := "/workspace"

	steps := []corev1.Container{}
	for _, l := range lifecycles.All() {
		if l == nil {
			continue
		}
		for _, s := range l.Steps {
			ss, err := o.createSteps(languageName, pipelineConfig, templateKind, s, container, dir)
			if err != nil {
				return err
			}
			steps = append(steps, ss...)
		}
	}
	name := "jx-task-" + languageName + "-" + templateKind
	task := &pipelineapi.Task{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "pipeline.knative.dev/v1alpha1",
			Kind:       "Task",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: kube.ToValidName(name),
		},
		Spec: pipelineapi.TaskSpec{
			Steps: steps,
		},
	}
	data, err := yaml.Marshal(task)
	if err != nil {
		return errors.Wrapf(err, "failed to marshal Task YAML")
	}
	fileName := o.OutputFile
	if fileName == "" {
		log.Infof("%s\n", string(data))
		return nil
	}
	err = ioutil.WriteFile(fileName, data, util.DefaultWritePermissions)
	if err != nil {
		return errors.Wrapf(err, "failed to save Task file %s", fileName)
	}
	log.Infof("generated Task at %s\n", util.ColorInfo(fileName))
	return nil
}

func (o *StepCreateTaskOptions) createSteps(languageName string, pipelineConfig *jenkinsfile.PipelineConfig, templateKind string, step *jenkinsfile.PipelineStep, containerName string, dir string) ([]corev1.Container, error) {

	steps := []corev1.Container{}

	if step.Container != "" {
		containerName = step.Container
	} else if step.Dir != "" {
		dir = step.Dir
	}
	if step.Command != "" {
		if containerName == "" {
			containerName = defaultContainerName
		}
		podTemplate := o.PodTemplates[containerName]
		if podTemplate == nil {
			o.MissingPodTemplates[containerName] = true
			podTemplate = o.PodTemplates[defaultContainerName]
		}
		containers := podTemplate.Spec.Containers
		if len(containers) == 0 {
			return steps, fmt.Errorf("No Containers for pod template %s", containerName)
		}
		c := containers[0]

		o.removeUnnecessaryVolumes(&c)
		o.removeUnnecessaryEnvVars(&c)

		c.Command = []string{"/bin/sh"}
		c.Args = []string{"-c", step.Command}

		if strings.HasPrefix(dir, "./") {
			dir = "/workspace" + strings.TrimPrefix(dir, ".")
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join("/workspace", dir)
		}
		c.WorkingDir = dir

		// TODO use different image based on if its jx or not?
		c.Image = "jenkinsxio/jx:latest"

		steps = append(steps, c)
	}
	for _, s := range step.Steps {
		childSteps, err := o.createSteps(languageName, pipelineConfig, templateKind, s, containerName, dir)
		if err != nil {
			return steps, err
		}
		steps = append(steps, childSteps...)
	}
	return steps, nil
}

func (o *StepCreateTaskOptions) discoverBuildPack(dir string, projectConfig *config.ProjectConfig) (string, error) {
	args := &InvokeDraftPack{
		Dir:             o.Dir,
		CustomDraftPack: o.Pack,
		ProjectConfig:   projectConfig,
		DisableAddFiles: true,
	}
	pack, err := o.invokeDraftPack(args)
	if err != nil {
		return pack, errors.Wrapf(err, "failed to discover task pack in dir %s", o.Dir)
	}
	return pack, nil
}

func (o *StepCreateTaskOptions) removeUnnecessaryVolumes(container *corev1.Container) {
	// for now let remove them all?
	container.VolumeMounts = nil
}

func (o *StepCreateTaskOptions) removeUnnecessaryEnvVars(container *corev1.Container) {
	envVars := []corev1.EnvVar{}
	for _, e := range container.Env {
		name := e.Name
		if strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "DOCKER_") || strings.HasPrefix(name, "XDG_") {
			continue
		}
		envVars = append(envVars, e)
	}
	container.Env = envVars
}
