package kubedeploy

import (
	"encoding/base64"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/pkg/api/v1"
	apis_batch_v1 "k8s.io/client-go/pkg/apis/batch/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/pkg/util"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/checkr/codeflow/server/agent"
	"github.com/checkr/codeflow/server/plugins"
	"github.com/extemporalgenome/slug"
	"github.com/google/shlex"
	"github.com/spf13/viper"
)

func genNamespaceName(suggestedEnvironment string, projectSlug string) string {
	if viper.IsSet("plugins.kubedeploy.environment") {
		return fmt.Sprintf("%s-%s", viper.GetString("plugins.kubedeploy.environment"), projectSlug)
	}
	return fmt.Sprintf("%s-%s", suggestedEnvironment, projectSlug)
}

func genDeploymentName(repoName string, serviceName string) string {
	return slug.Slug(repoName) + "-" + serviceName
}

func genOneShortServiceName(repoName string, serviceName string) string {
	return "os-" + slug.Slug(repoName) + "-" + serviceName
}

func (x *KubeDeploy) sendDDResponse(e agent.Event, services []plugins.Service, state plugins.State, failureMessage string) {
	data := e.Payload.(plugins.DockerDeploy)
	data.Action = plugins.Status
	data.State = state
	data.Services = services
	data.StateMessage = failureMessage
	event := e.NewEvent(data, nil)

	x.events <- event
}

func (x *KubeDeploy) sendDDSuccessResponse(e agent.Event, services []plugins.Service) {
	x.sendDDResponse(e, services, plugins.Complete, "")
}

func (x *KubeDeploy) sendDDErrorResponse(e agent.Event, services []plugins.Service, failureMessage string) {
	x.sendDDResponse(e, services, plugins.Failed, failureMessage)
}

func (x *KubeDeploy) sendDDInProgress(e agent.Event, services []plugins.Service, message string) {
	x.sendDDResponse(e, services, plugins.Running, message)
}

func secretifyDockerCred() string {
	encodeMe := fmt.Sprintf("%s:%s",
		viper.GetString("plugins.docker_build.registry_username"),
		viper.GetString("plugins.docker_build.registry_password"))
	encodeResult := []byte(encodeMe)
	authField := base64.StdEncoding.EncodeToString(encodeResult)
	jsonFilled := fmt.Sprintf("{\"%s\":{\"username\":\"%s\",\"password\":\"%s\",\"email\":\"%s\",\"auth\":\"%s\"}}",
		viper.GetString("plugins.docker_build.registry_host"),
		viper.GetString("plugins.docker_build.registry_username"),
		viper.GetString("plugins.docker_build.registry_password"),
		viper.GetString("plugins.docker_build.registry_user_email"),
		authField,
	)
	return jsonFilled
}

func (x *KubeDeploy) createDockerIOSecretIfNotExists(namespace string, coreInterface corev1.CoreV1Interface) error {
	// Load up the docker-io secrets for image pull if not exists
	_, dockerIOSecretErr := coreInterface.Secrets(namespace).Get("docker-io", meta_v1.GetOptions{})
	if dockerIOSecretErr != nil {
		if errors.IsNotFound(dockerIOSecretErr) {
			log.Printf("docker-io secret not found for %s, creating.", namespace)
			secretMap := map[string]string{
				".dockercfg": secretifyDockerCred(),
			}
			_, createDockerIOSecretErr := coreInterface.Secrets(namespace).Create(&v1.Secret{
				TypeMeta: meta_v1.TypeMeta{
					Kind:       "Secret",
					APIVersion: "v1",
				},
				ObjectMeta: meta_v1.ObjectMeta{
					Name:      "docker-io",
					Namespace: namespace,
				},
				StringData: secretMap,
				Type:       v1.SecretTypeDockercfg,
			})
			if createDockerIOSecretErr != nil {
				log.Printf("Error '%s' creating docker-io secret for %s.", createDockerIOSecretErr, namespace)
				return createDockerIOSecretErr
			}
		} else {
			log.Printf("Error unhandled '%s' while attempting to lookup docker-io secret.", dockerIOSecretErr)
			return dockerIOSecretErr
		}
	}
	return nil
}

func (x *KubeDeploy) createNamespaceIfNotExists(namespace string, coreInterface corev1.CoreV1Interface) error {
	// Create namespace if it does not exist.
	_, nameGetErr := coreInterface.Namespaces().Get(namespace, meta_v1.GetOptions{})
	if nameGetErr != nil {
		if errors.IsNotFound(nameGetErr) {
			log.Printf("Namespace %s does not yet exist, creating.", namespace)
			namespaceParams := &v1.Namespace{
				TypeMeta: meta_v1.TypeMeta{
					Kind:       "Namespace",
					APIVersion: "v1",
				},
				ObjectMeta: meta_v1.ObjectMeta{
					Name: namespace,
				},
			}
			_, createNamespaceErr := coreInterface.Namespaces().Create(namespaceParams)
			if createNamespaceErr != nil {
				log.Printf("Error '%s' creating namespace %s", createNamespaceErr, namespace)
				return createNamespaceErr
			}
			log.Printf("Namespace created: %s", namespace)
		} else {
			log.Printf("Unhandled error occured looking up namespace %s: '%s'", namespace, nameGetErr)
			return nameGetErr
		}
	}
	return nil
}

func getContainerPorts(service plugins.Service) []v1.ContainerPort {
	var deployPorts []v1.ContainerPort

	// ContainerPorts for the deployment
	for _, cPort := range service.Listeners {
		// Build the deployments containerports array
		newContainerPort := v1.ContainerPort{
			ContainerPort: cPort.Port,
			Protocol:      v1.Protocol(cPort.Protocol),
		}
		deployPorts = append(deployPorts, newContainerPort)
	}

	return deployPorts
}

func (x *KubeDeploy) doDeploy(e agent.Event) error {
	// Codeflow will load the kube config from a file, specified by CF_PLUGINS_KUBEDEPLOY_KUBECONFIG environment variable
	kubeconfig := viper.GetString("plugins.kubedeploy.kubeconfig")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)

	if err != nil {
		panic(err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Println("Error getting cluster config.")
	}

	data := e.Payload.(plugins.DockerDeploy)
	x.sendDDInProgress(e, data.Services, "Deploy in-progress")
	namespace := genNamespaceName(data.Environment, data.Project.Slug)
	coreInterface := clientset.Core()

	successfulDeploys := 0
	timeout := e.Payload.(plugins.DockerDeploy).Timeout
	// Set default timeout to 600 seconds if not specified.
	if timeout == 0 {
		timeout = 600
	}
	curTime := 0

	createNamespaceErr := x.createNamespaceIfNotExists(namespace, coreInterface)
	if createNamespaceErr != nil {
		x.sendDDErrorResponse(e, data.Services, createNamespaceErr.Error())
		return nil
	}

	createDockerIOSecretErr := x.createDockerIOSecretIfNotExists(namespace, coreInterface)
	if createDockerIOSecretErr != nil {
		x.sendDDErrorResponse(e, data.Services, createDockerIOSecretErr.Error())
		return nil
	}

	// Create secrets for this deploy
	var secretMap map[string]string
	secretMap = make(map[string]string)
	var myEnvVars []v1.EnvVar

	// This map is used in to create the secrets themselves
	for _, secret := range data.Secrets {
		secretMap[secret.Key] = secret.Value
	}

	secretParams := &v1.Secret{
		TypeMeta: meta_v1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		ObjectMeta: meta_v1.ObjectMeta{
			GenerateName: fmt.Sprintf("%v-", data.Project.Slug),
			Namespace:    namespace,
		},
		StringData: secretMap,
		Type:       v1.SecretTypeOpaque,
	}

	secretResult, secErr := coreInterface.Secrets(namespace).Create(secretParams)
	if secErr != nil {
		failMessage := fmt.Sprintf("Error '%s' creating secret %s", secErr, data.Project.Slug)
		x.sendDDErrorResponse(e, data.Services, failMessage)
		return nil
	}
	secretName := secretResult.Name
	log.Printf("Secrets created: %s", secretName)

	// This is for building the configuration to use the secrets from inside the deployment
	// as ENVs
	for _, secret := range data.Secrets {
		if secret.Type == plugins.Env || secret.Type == plugins.ProtectedEnv {
			newEnv := v1.EnvVar{
				Name: secret.Key,
				ValueFrom: &v1.EnvVarSource{
					SecretKeyRef: &v1.SecretKeySelector{
						LocalObjectReference: v1.LocalObjectReference{
							Name: secretName,
						},
						Key: secret.Key,
					},
				},
			}
			myEnvVars = append(myEnvVars, newEnv)
		}
	}
	// as Files
	var volumeMounts []v1.VolumeMount
	var deployVolumes []v1.Volume
	var volumeSecretItems []v1.KeyToPath
	volumeMounts = append(volumeMounts, v1.VolumeMount{
		Name:      secretName,
		MountPath: "/etc/secrets",
		ReadOnly:  true,
	})
	for _, secret := range data.Secrets {
		if secret.Type == plugins.File {
			volumeSecretItems = append(volumeSecretItems, v1.KeyToPath{
				Path: secret.Key,
				Key:  secret.Key,
				Mode: util.Int32Ptr(256),
			})
		}
	}
	secretVolume := v1.SecretVolumeSource{
		SecretName:  secretName,
		Items:       volumeSecretItems,
		DefaultMode: util.Int32Ptr(256),
	}

	// Add the secrets
	deployVolumes = append(deployVolumes, v1.Volume{
		Name: secretName,
		VolumeSource: v1.VolumeSource{
			Secret: &secretVolume,
		},
	})

	// Do update/create of deployments and services
	depInterface := clientset.Extensions()
	batchv1DepInterface := clientset.BatchV1()

	// Validate we have some services to deploy
	if len(data.Services) == 0 {
		failMessage := fmt.Sprintf("ERROR: Zero services were found in the deploy message.")
		x.sendDDErrorResponse(e, data.Services, failMessage)
		return nil
	}

	// Codeflow docker building container requires docker socket.
	if data.Project.Slug == "checkr-codeflow" {
		deployVolumes = append(deployVolumes, v1.Volume{
			Name: "dockersocket",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/var/run/docker.sock",
				},
			},
		})
		volumeMounts = append(volumeMounts, v1.VolumeMount{
			Name:      "dockersocket",
			ReadOnly:  false,
			MountPath: "/var/run/docker.sock",
		})
	}

	// prioritize one-shot services over deployments
	// because migrations (which are one-shot jobs) should be
	// run before app code deployments

	var deploymentServices []plugins.Service
	var oneShotServices []plugins.Service

	for _, service := range data.Services {
		if service.OneShot == true {
			oneShotServices = append(oneShotServices, service)
		} else {
			deploymentServices = append(deploymentServices, service)
		}
	}

	for index, service := range oneShotServices {
		oneShotServiceName := strings.ToLower(genOneShortServiceName(data.Project.Slug, service.Name))

		// Check and delete any completed or failed jobs, and delete respective pods
		existingJobs, err := batchv1DepInterface.Jobs(namespace).List(meta_v1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s", "app", oneShotServiceName)})
		if err != nil {
			log.Printf("Failed to load existing jobs with label app=%s, with error: %s", oneShotServiceName, err)
			oneShotServices[index].State = plugins.Failed
			oneShotServices[index].StateMessage = fmt.Sprintf("Error creating job: %s : %s", oneShotServiceName, err)
			x.sendDDErrorResponse(e, oneShotServices, oneShotServices[index].StateMessage)
			return nil
		}

		for index, job := range existingJobs.Items {
			if (job.Status.Active == 0 && job.Status.Failed == 0 && job.Status.Succeeded == 0) || job.Status.Active > 0 {
				oneShotServices[index].State = plugins.Failed
				oneShotServices[index].StateMessage = fmt.Sprintf("Error previous job is still active: %s", job.Name)
				x.sendDDErrorResponse(e, oneShotServices, oneShotServices[index].StateMessage)
				return nil
			}

			// delete old job
			gracePeriod := int64(0)
			isOrphan := true
			deleteOptions := meta_v1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
				OrphanDependents:   &isOrphan,
			}
			batchv1DepInterface.Jobs(namespace).Delete(job.Name, &deleteOptions)
		}

		// Command parsing into entrypoint vs. args
		commandArray, _ := shlex.Split(service.Command)

		// Node selector
		var nodeSelector map[string]string
		if viper.IsSet("plugins.kubedeploy.node_selector") {
			arrayKeyValue := strings.SplitN(viper.GetString("plugins.kubedeploy.node_selector"), "=", 2)
			nodeSelector = map[string]string{arrayKeyValue[0]: arrayKeyValue[1]}
		}

		terminationGracePeriodSeconds := service.Spec.TerminationGracePeriodSeconds

		podTemplateSpec := v1.PodTemplateSpec{
			ObjectMeta: meta_v1.ObjectMeta{
				Name:   oneShotServiceName,
				Labels: map[string]string{"app": oneShotServiceName},
			},
			Spec: v1.PodSpec{
				NodeSelector:                  nodeSelector,
				TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
				ImagePullSecrets: []v1.LocalObjectReference{
					{
						Name: "docker-io",
					},
				},
				Containers: []v1.Container{
					{
						Name:  strings.ToLower(service.Name),
						Image: data.Docker.Image,
						Args:  commandArray,
						Resources: v1.ResourceRequirements{
							Limits: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse(service.Spec.CpuLimit),
								v1.ResourceMemory: resource.MustParse(service.Spec.MemoryLimit),
							},
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse(service.Spec.CpuRequest),
								v1.ResourceMemory: resource.MustParse(service.Spec.MemoryRequest),
							},
						},
						ImagePullPolicy: v1.PullIfNotPresent,
						Env:             myEnvVars,
						VolumeMounts:    volumeMounts,
					},
				},
				Volumes:       deployVolumes,
				RestartPolicy: v1.RestartPolicyNever,
				DNSPolicy:     v1.DNSClusterFirst,
			},
		}

		numParallelPods := int32(1)
		numCompletionsToTerminate := int32(1)

		var jobParams *apis_batch_v1.Job
		jobParams = &apis_batch_v1.Job{
			TypeMeta: meta_v1.TypeMeta{
				Kind:       "Job",
				APIVersion: "batch/v1",
			},
			ObjectMeta: meta_v1.ObjectMeta{
				GenerateName: oneShotServiceName,
				Labels:       map[string]string{"app": oneShotServiceName},
			},
			Spec: apis_batch_v1.JobSpec{
				Parallelism: &numParallelPods,
				Completions: &numCompletionsToTerminate,
				Template:    podTemplateSpec,
			},
		}

		createdJob, err := batchv1DepInterface.Jobs(namespace).Create(jobParams)
		if err != nil {
			log.Printf("Failed to create service job %s, with error: %s", createdJob.Name, err)
			oneShotServices[index].State = plugins.Failed
			oneShotServices[index].StateMessage = fmt.Sprintf("Failed to create job %s, with error: %s", createdJob.Name, err)
			x.sendDDErrorResponse(e, oneShotServices, oneShotServices[index].StateMessage)
			return nil
		}

		// Loop and block any other jobs/ deployments from running until
		// the current job is terminated
		for {
			job, err := batchv1DepInterface.Jobs(namespace).Get(createdJob.Name, meta_v1.GetOptions{})
			if err != nil {
				log.Printf("Error '%s' fetching job status for %s", err, createdJob.Name)
				time.Sleep(5 * time.Second)
				continue
			}

			log.Printf("Job Status: Active: %v ; Succeeded: %v, Failed: %v \n", job.Status.Active, job.Status.Succeeded, job.Status.Failed)

			// Container is still creating
			if job.Status.Active == 0 && job.Status.Failed == 0 && job.Status.Succeeded == 0 {
				time.Sleep(5 * time.Second)
				continue
			}

			if job.Status.Active == int32(0) {
				// Check for success
				if job.Status.Succeeded > int32(0) {
					oneShotServices[index].State = plugins.Complete
					break
				} else {
					// Job has failed!
					oneShotServices[index].State = plugins.Failed
					oneShotServices[index].StateMessage = fmt.Sprintf("Error job has failed %s", oneShotServiceName)
					x.sendDDErrorResponse(e, oneShotServices, oneShotServices[index].StateMessage)
					return nil
				}
			}

			time.Sleep(5 * time.Second)
		}

	}

	for index, service := range deploymentServices {
		deploymentName := genDeploymentName(data.Project.Slug, service.Name)
		deployPorts := getContainerPorts(service)

		// Support ready and liveness probes
		var readyProbe v1.Probe
		var liveProbe v1.Probe
		var deployStrategy v1beta1.DeploymentStrategy
		if len(service.Listeners) >= 1 && service.Listeners[0].Protocol == "TCP" {
			// If the service is TCP, use a TCP Probe
			myPort := service.Listeners[0].Port
			readyProbe = v1.Probe{
				InitialDelaySeconds: 5,
				PeriodSeconds:       10,
				SuccessThreshold:    1,
				FailureThreshold:    3,
				TimeoutSeconds:      1,
				Handler: v1.Handler{
					TCPSocket: &v1.TCPSocketAction{
						Port: intstr.IntOrString{IntVal: myPort},
					},
				},
			}
			liveProbe = v1.Probe{
				InitialDelaySeconds: 15,
				PeriodSeconds:       20,
				SuccessThreshold:    1,
				FailureThreshold:    3,
				TimeoutSeconds:      1,
				Handler: v1.Handler{
					TCPSocket: &v1.TCPSocketAction{
						Port: intstr.IntOrString{IntVal: myPort},
					},
				},
			}
			deployStrategy = v1beta1.DeploymentStrategy{
				Type: v1beta1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &v1beta1.RollingUpdateDeployment{
					MaxUnavailable: &intstr.IntOrString{
						Type:   intstr.String,
						StrVal: "30%",
					},
					MaxSurge: &intstr.IntOrString{
						Type:   intstr.String,
						StrVal: "60%",
					},
				},
			}
		} else {
			// If the service is non-TCP or has no ports use a simple exec probe
			runThis := []string{"/bin/true"}
			readyProbe = v1.Probe{
				Handler: v1.Handler{
					Exec: &v1.ExecAction{
						Command: runThis,
					},
				},
			}
			liveProbe = v1.Probe{
				InitialDelaySeconds: 15,
				PeriodSeconds:       20,
				SuccessThreshold:    1,
				FailureThreshold:    3,
				TimeoutSeconds:      1,
				Handler: v1.Handler{
					Exec: &v1.ExecAction{
						Command: runThis,
					},
				},
			}
			deployStrategy = v1beta1.DeploymentStrategy{
				Type: "Recreate",
			}
		}

		// Deployment
		replicas := int32(service.Replicas)
		if service.Action == plugins.Destroy {
			replicas = 0
		}

		// Command parsing into entrypoint vs. args
		commandArray, _ := shlex.Split(service.Command)

		// Node selector
		var nodeSelector map[string]string
		if viper.IsSet("plugins.kubedeploy.node_selector") {
			arrayKeyValue := strings.SplitN(viper.GetString("plugins.kubedeploy.node_selector"), "=", 2)
			nodeSelector = map[string]string{arrayKeyValue[0]: arrayKeyValue[1]}
		}

		var revisionHistoryLimit int32 = 10
		terminationGracePeriodSeconds := service.Spec.TerminationGracePeriodSeconds

		podTemplateSpec := v1.PodTemplateSpec{
			ObjectMeta: meta_v1.ObjectMeta{
				Name:   deploymentName,
				Labels: map[string]string{"app": deploymentName},
			},
			Spec: v1.PodSpec{
				NodeSelector:                  nodeSelector,
				TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
				ImagePullSecrets: []v1.LocalObjectReference{
					{
						Name: "docker-io",
					},
				},
				Containers: []v1.Container{
					{
						Name:  strings.ToLower(service.Name),
						Image: data.Docker.Image,
						Ports: deployPorts,
						Args:  commandArray,
						Resources: v1.ResourceRequirements{
							Limits: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse(service.Spec.CpuLimit),
								v1.ResourceMemory: resource.MustParse(service.Spec.MemoryLimit),
							},
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse(service.Spec.CpuRequest),
								v1.ResourceMemory: resource.MustParse(service.Spec.MemoryRequest),
							},
						},
						ImagePullPolicy: v1.PullIfNotPresent,
						Env:             myEnvVars,
						ReadinessProbe:  &readyProbe,
						LivenessProbe:   &liveProbe,
						VolumeMounts:    volumeMounts,
					},
				},
				Volumes:       deployVolumes,
				RestartPolicy: v1.RestartPolicyAlways,
				DNSPolicy:     v1.DNSClusterFirst,
			},
		}

		var deployParams *v1beta1.Deployment

		deployParams = &v1beta1.Deployment{
			TypeMeta: meta_v1.TypeMeta{
				Kind:       "Deployment",
				APIVersion: "extensions/v1beta1",
			},
			ObjectMeta: meta_v1.ObjectMeta{
				Name: deploymentName,
			},
			Spec: v1beta1.DeploymentSpec{
				ProgressDeadlineSeconds: util.Int32Ptr(300),
				Replicas:                &replicas,
				Strategy:                deployStrategy,
				RevisionHistoryLimit:    &revisionHistoryLimit,
				Template:                podTemplateSpec,
			},
		}

		var err error
		log.Printf("Getting list of deployments/ jobs matching %s", deploymentName)
		_, err = depInterface.Deployments(namespace).Get(deploymentName, meta_v1.GetOptions{})
		var myError error
		if err != nil {
			// Create deployment if it does not exist
			log.Printf("Existing deployment not found for %s. requested action: %s.", deploymentName, service.Action)
			// Sanity check that we were told to create this service or error out.

			_, myError = depInterface.Deployments(namespace).Create(deployParams)
			if myError != nil {
				// send failed status
				log.Printf("Failed to create service deployment %s, with error: %s", deploymentName, myError)
				deploymentServices[index].State = plugins.Failed
				deploymentServices[index].StateMessage = fmt.Sprintf("Error creating deployment: %s", myError)
				// shorten the timeout in this case so that we can fail without waiting
				curTime = timeout
			}
		} else {
			// Deployment exists, update deployment with new configuration
			_, myError = depInterface.Deployments(namespace).Update(deployParams)
			if myError != nil {
				log.Printf("Failed to update service deployment %s, with error: %s", deploymentName, myError)
				deploymentServices[index].State = plugins.Failed
				deploymentServices[index].StateMessage = fmt.Sprintf("Failed to update deployment %s, with error: %s", deploymentName, myError)
				// shorten the timeout in this case so that we can fail without waiting
				curTime = timeout
			}
		}

	} // All service deployments initiated.

	log.Printf("Waiting %d seconds for deployment to succeed.", timeout)
	for i := range deploymentServices {
		deploymentServices[i].State = plugins.Waiting
	}

	if len(deploymentServices) > 0 {
		// Check status of all deployments till the succeed or timeout.
		replicaFailures := 0
		for {
			for index, service := range deploymentServices {
				deploymentName := strings.ToLower(genDeploymentName(data.Project.Slug, service.Name))
				deployment, err := depInterface.Deployments(namespace).Get(deploymentName, meta_v1.GetOptions{})
				if err != nil {
					log.Printf("Error '%s' fetching deployment status for %s", err, deploymentName)
					continue
				}
				log.Printf("Waiting for %s; ObservedGeneration: %d, Generation: %d, UpdatedReplicas: %d, Replicas: %d, AvailableReplicas: %d, UnavailableReplicas: %d", deploymentName, deployment.Status.ObservedGeneration, deployment.ObjectMeta.Generation, deployment.Status.UpdatedReplicas, *deployment.Spec.Replicas, deployment.Status.AvailableReplicas, deployment.Status.UnavailableReplicas)

				if deployment.Status.ObservedGeneration >= deployment.ObjectMeta.Generation && deployment.Status.UpdatedReplicas == *deployment.Spec.Replicas && deployment.Status.AvailableReplicas >= deployment.Status.UpdatedReplicas && deployment.Status.UnavailableReplicas == 0 {
					// deployment success
					deploymentServices[index].State = plugins.Complete
					/// AH HA!! haha
					successfulDeploys = 0
					for _, d := range deploymentServices {
						if d.State == plugins.Complete {
							successfulDeploys++
						}
					}
					log.Printf("%s deploy: %d of %d deployments successful.", deploymentName, successfulDeploys, len(deploymentServices))
				}

				//for _, condition := range deployment.Status.Conditions {
				//	if condition.Type == v1beta1.DeploymentReplicaFailure || (condition.Type == v1beta1.DeploymentProgressing && condition.Status == v1.ConditionFalse) {
				//		replicaFailures += 1
				//		data.Services[index].State = plugins.Failed
				//		data.Services[index].StateMessage = condition.Message
				//		log.Printf("%s failed to start: %v (%v)", deploymentName, condition.Message, condition.Reason)
				//	}
				//}

				if successfulDeploys == len(deploymentServices) {
					// all success!
					log.Printf("All deployments successful.")
					x.sendDDSuccessResponse(e, append(deploymentServices, oneShotServices...))

					// cleanup Orphans! (these are deployments leftover from rename or etc.)
					allDeploymentsList, listErr := depInterface.Deployments(namespace).List(meta_v1.ListOptions{})
					if listErr != nil {
						// If we can't list the deployments just return.  We have already sent the success message.
						log.Printf("Fatal Error listing deployments during cleanup.  %s", listErr)
						return nil
					}
					var foundIt bool
					var orphans []v1beta1.Deployment
					for _, deployment := range allDeploymentsList.Items {
						foundIt = false
						for _, service := range data.Services {
							if deployment.Name == genDeploymentName(data.Project.Slug, service.Name) {
								foundIt = true
							}
						}
						if foundIt == false {
							orphans = append(orphans, deployment)
						}
					}

					// Preload list of all replica sets
					repSets, repErr := depInterface.ReplicaSets(namespace).List(meta_v1.ListOptions{})
					if repErr != nil {
						log.Printf("Error retrieving list of replicasets for %s", namespace)
						return nil
					}

					// Preload list of all pods
					allPods, podErr := coreInterface.Pods(namespace).List(meta_v1.ListOptions{})
					if podErr != nil {
						log.Printf("Error retrieving list of pods for %s", namespace)
						return nil
					}

					// Delete the deployments
					for _, deleteThis := range orphans {
						matched, _ := regexp.MatchString("^keep", deleteThis.Name)
						if matched {
							continue
						}

						log.Printf("Deleting deployment orphan: %s", deleteThis.Name)
						err := depInterface.Deployments(namespace).Delete(deleteThis.Name, &meta_v1.DeleteOptions{})
						if err != nil {
							log.Printf("Error when deleting: %s", err)
						}

						// Delete the replicasets (cascade)
						for _, repSet := range repSets.Items {
							if repSet.ObjectMeta.Labels["app"] == deleteThis.Name {
								log.Printf("Deleting replicaset orphan: %s", repSet.Name)
								err := depInterface.ReplicaSets(namespace).Delete(repSet.Name, &meta_v1.DeleteOptions{})
								if err != nil {
									log.Printf("Error '%s' while deleting replica set %s", err, repSet.Name)
								}
							}
						}

						// Delete the pods (cascade) or scale down the repset
						for _, pod := range allPods.Items {
							if pod.ObjectMeta.Labels["app"] == deleteThis.Name {
								log.Printf("Deleting pod orphan: %s", pod.Name)
								err := coreInterface.Pods(namespace).Delete(pod.Name, &meta_v1.DeleteOptions{})
								if err != nil {
									log.Printf("Error '%s' while deleting pod %s", err, pod.Name)
								}
							}
						}
					}

					// Already sent success, return.
					return nil
				}
			}

			if curTime >= timeout || replicaFailures > 1 {
				// timeout and get ready to rollback!
				log.Printf("Error, timeout reached waiting for all deployments to succeed.")
				x.sendDDErrorResponse(e, append(deploymentServices, oneShotServices...), "Error, timeout reached waiting for all deployments to succeed.")
				break
			}
			time.Sleep(5 * time.Second)
			curTime += 5
		}

	}

	x.sendDDSuccessResponse(e, oneShotServices)

	return nil
}
