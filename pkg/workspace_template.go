package v1

import (
	"database/sql"
	"encoding/json"
	goerrors "errors"
	"fmt"
	sq "github.com/Masterminds/squirrel"
	wfv1 "github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	"github.com/asaskevich/govalidator"
	"github.com/onepanelio/core/pkg/util"
	"github.com/onepanelio/core/pkg/util/env"
	"github.com/onepanelio/core/pkg/util/pagination"
	"github.com/onepanelio/core/pkg/util/ptr"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	networking "istio.io/api/networking/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net/http"
	"sigs.k8s.io/yaml"
	"strings"
)

// createWorkspaceTemplateVersionDB creates a workspace template version in the database.
func createWorkspaceTemplateVersionDB(tx sq.BaseRunner, template *WorkspaceTemplate) (err error) {
	err = sb.Insert("workspace_template_versions").
		SetMap(sq.Eq{
			"version":               template.Version,
			"is_latest":             template.IsLatest,
			"manifest":              template.Manifest,
			"workspace_template_id": template.ID,
			"labels":                template.Labels,
		}).
		Suffix("RETURNING id").
		RunWith(tx).
		QueryRow().
		Scan(&template.ID)

	return
}

// markWorkspaceTemplateVersionsOutdatedDB updates all of the workspace template versions in db so is_latest is false
// given the workspaceTemplateID
func markWorkspaceTemplateVersionsOutdatedDB(tx sq.BaseRunner, workspaceTemplateID uint64) (err error) {
	_, err = sb.Update("workspace_template_versions").
		SetMap(sq.Eq{"is_latest": false}).
		Where(sq.Eq{
			"workspace_template_id": workspaceTemplateID,
			"is_latest":             true,
		}).
		RunWith(tx).
		Exec()

	return
}

// createLatestWorkspaceTemplateVersionDB creates a new workspace template version and marks all previous versions as not latest.
func createLatestWorkspaceTemplateVersionDB(tx sq.BaseRunner, template *WorkspaceTemplate) (err error) {
	if template == nil {
		return fmt.Errorf("workspaceTemplate is nil")
	}
	if template.ID < 1 {
		return fmt.Errorf("workspaceTemplate.ID is not set")
	}

	err = markWorkspaceTemplateVersionsOutdatedDB(tx, template.ID)
	if err != nil {
		return
	}

	template.IsLatest = true

	return createWorkspaceTemplateVersionDB(tx, template)
}

func parseWorkspaceSpec(template string) (spec *WorkspaceSpec, err error) {
	err = yaml.UnmarshalStrict([]byte(template), &spec)

	return
}

func generateRuntimeParameters(config SystemConfig) (parameters []Parameter, err error) {
	parameters = make([]Parameter, 0)

	// Host
	parameters = append(parameters, Parameter{
		Name:  "sys-host",
		Value: config.Domain(),
		Type:  "input.hidden",
	})

	// Node pool parameter and options
	nodePoolOptions, err := config.NodePoolOptions()
	if err != nil {
		return nil, err
	}
	if len(nodePoolOptions) == 0 {
		return nil, fmt.Errorf("no node pool options in config")
	}
	var options []*ParameterOption
	for _, option := range nodePoolOptions {
		options = append(options, &ParameterOption{
			Name:  option.Name,
			Value: option.Value,
		})
	}

	parameters = append(parameters, Parameter{
		Name:  "sys-node-pool-label",
		Value: config.NodePoolLabel(),
		Type:  "input.hidden",
	})

	parameters = append(parameters, Parameter{
		Name:        "sys-node-pool",
		Value:       ptr.String(options[0].Value),
		Type:        "select.select",
		Options:     options,
		DisplayName: ptr.String("Node pool"),
		Hint:        ptr.String("Name of node pool or group"),
		Required:    true,
	})

	return
}

func generateArguments(spec *WorkspaceSpec, config SystemConfig) (err error) {
	systemParameters := make([]Parameter, 0)
	// Resource action parameter
	systemParameters = append(systemParameters, Parameter{
		Name:        "sys-name",
		Type:        "input.text",
		Value:       ptr.String("name"),
		DisplayName: ptr.String("Workspace name"),
		Hint:        ptr.String("Must be between 3-30 characters, contain only alphanumeric or `-` characters"),
		Required:    true,
	})

	// TODO: These can be removed when lint validation of workflows work
	// Resource action parameter
	systemParameters = append(systemParameters, Parameter{
		Name:  "sys-resource-action",
		Value: ptr.String("apply"),
		Type:  "input.hidden",
	})
	// Workspace action
	systemParameters = append(systemParameters, Parameter{
		Name:  "sys-workspace-action",
		Value: ptr.String("create"),
		Type:  "input.hidden",
	})

	// UID placeholder
	systemParameters = append(systemParameters, Parameter{
		Name:  "sys-uid",
		Value: ptr.String("uid"),
		Type:  "input.hidden",
	})

	runtimeParameters, err := generateRuntimeParameters(config)
	if err != nil {
		return
	}
	systemParameters = append(systemParameters, runtimeParameters...)

	if spec.Arguments == nil {
		spec.Arguments = &Arguments{
			Parameters: []Parameter{},
		}
	}
	spec.Arguments.Parameters = append(systemParameters, spec.Arguments.Parameters...)

	systemVolumeParameters := make([]Parameter, 0)
	// Map all the volumeClaimTemplates that have storage set
	volumeStorageQuantityIsSet := make(map[string]bool)
	for _, v := range spec.VolumeClaimTemplates {
		if v.Spec.Resources.Requests != nil {
			volumeStorageQuantityIsSet[v.ObjectMeta.Name] = true
		}
	}
	// Volume size parameters
	volumeClaimsMapped := make(map[string]bool)
	for _, c := range spec.Containers {
		for _, v := range c.VolumeMounts {
			// Skip if already mapped or storage size is set
			if volumeClaimsMapped[v.Name] || volumeStorageQuantityIsSet[v.Name] {
				continue
			}

			systemVolumeParameters = append(systemVolumeParameters, Parameter{
				Name:        fmt.Sprintf("sys-%v-volume-size", v.Name),
				Type:        "input.number",
				Value:       ptr.String("20480"),
				DisplayName: ptr.String(fmt.Sprintf("Disk size for \"%v\"", v.Name)),
				Hint:        ptr.String(fmt.Sprintf("Disk size in MB for volume mounted at `%v`", v.MountPath)),
				Required:    true,
			})

			volumeClaimsMapped[v.Name] = true
		}
	}

	spec.Arguments.Parameters = append(spec.Arguments.Parameters, systemVolumeParameters...)

	return
}

func createServiceManifest(spec *WorkspaceSpec) (serviceManifest string, err error) {
	service := corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "{{workflow.parameters.sys-uid}}",
		},
		Spec: corev1.ServiceSpec{
			Ports: spec.Ports,
			Selector: map[string]string{
				"app": "{{workflow.parameters.sys-uid}}",
			},
		},
	}
	serviceManifestBytes, err := yaml.Marshal(service)
	if err != nil {
		return
	}
	serviceManifest = string(serviceManifestBytes)

	return
}

func createVirtualServiceManifest(spec *WorkspaceSpec) (virtualServiceManifest string, err error) {
	for _, h := range spec.Routes {
		for _, r := range h.Route {
			r.Destination.Host = "{{workflow.parameters.sys-uid}}"
		}
	}
	virtualService := map[string]interface{}{
		"apiVersion": "networking.istio.io/v1alpha3",
		"kind":       "VirtualService",
		"metadata": metav1.ObjectMeta{
			Name: "{{workflow.parameters.sys-uid}}",
		},
		"spec": networking.VirtualService{
			Http:     spec.Routes,
			Gateways: []string{"istio-system/ingressgateway"},
			Hosts:    []string{"{{workflow.parameters.sys-host}}"},
		},
	}

	virtualServiceManifestBytes, err := yaml.Marshal(virtualService)
	if err != nil {
		return
	}
	virtualServiceManifest = string(virtualServiceManifestBytes)

	return
}

func createStatefulSetManifest(spec *WorkspaceSpec, config map[string]string, services []*Service) (statefulSetManifest string, err error) {
	var volumeClaims []map[string]interface{}
	volumeClaimsMapped := make(map[string]bool)
	// Add volumeClaims that the user has added first
	for _, v := range spec.VolumeClaimTemplates {
		if volumeClaimsMapped[v.ObjectMeta.Name] {
			continue
		}

		// Use the `onepanel` storage class instead of default
		if v.Spec.StorageClassName == nil {
			v.Spec.StorageClassName = ptr.String("onepanel")
		}
		// Check if storage is set or if it needs to be dynamic
		var storage interface{} = fmt.Sprintf("{{workflow.parameters.sys-%v-volume-size}}Mi", v.Name)
		if v.Spec.Resources.Requests != nil {
			storage = v.Spec.Resources.Requests["storage"]
		}
		volumeClaims = append(volumeClaims, map[string]interface{}{
			"metadata": metav1.ObjectMeta{
				Name: v.ObjectMeta.Name,
			},
			"spec": map[string]interface{}{
				"accessModes":      v.Spec.AccessModes,
				"storageClassName": v.Spec.StorageClassName,
				"resources": map[string]interface{}{
					"requests": map[string]interface{}{
						"storage": storage,
					},
				},
			},
		})

		volumeClaimsMapped[v.ObjectMeta.Name] = true
	}

	// Don't generate a volumeClaimTemplate for system volumes volumeMounts
	volumeClaimsMapped["sys-dshm"] = true
	volumeClaimsMapped["sys-namespace-config"] = true

	// Automatically map the remaining ones
	for i, c := range spec.Containers {
		container := &spec.Containers[i]
		env.AddDefaultEnvVarsToContainer(container)
		env.PrependEnvVarToContainer(container, "ONEPANEL_API_URL", config["ONEPANEL_API_URL"])
		env.PrependEnvVarToContainer(container, "ONEPANEL_FQDN", config["ONEPANEL_FQDN"])
		env.PrependEnvVarToContainer(container, "ONEPANEL_DOMAIN", config["ONEPANEL_DOMAIN"])
		env.PrependEnvVarToContainer(container, "ONEPANEL_PROVIDER", config["ONEPANEL_PROVIDER"])
		env.PrependEnvVarToContainer(container, "ONEPANEL_RESOURCE_NAMESPACE", "{{workflow.namespace}}")
		env.PrependEnvVarToContainer(container, "ONEPANEL_RESOURCE_UID", "{{workflow.parameters.sys-name}}")

		for _, service := range services {
			envName := fmt.Sprintf("ONEPANEL_SERVICES_%v_API_URL", strings.ToUpper(service.Name))
			env.PrependEnvVarToContainer(container, envName, service.URL)
		}

		for _, v := range c.VolumeMounts {
			if volumeClaimsMapped[v.Name] {
				continue
			}

			volumeClaims = append(volumeClaims, map[string]interface{}{
				"metadata": metav1.ObjectMeta{
					Name: v.Name,
				},
				"spec": map[string]interface{}{
					"accessModes": []corev1.PersistentVolumeAccessMode{
						"ReadWriteOnce",
					},
					"storageClassName": ptr.String("onepanel"),
					"resources": map[string]interface{}{
						"requests": map[string]string{
							"storage": fmt.Sprintf("{{workflow.parameters.sys-%v-volume-size}}Mi", v.Name),
						},
					},
				},
			})

			volumeClaimsMapped[v.Name] = true
		}

		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      "sys-dshm",
			MountPath: "/dev/shm",
		})
	}

	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app": "{{workflow.parameters.sys-uid}}",
			},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{
				"{{workflow.parameters.sys-node-pool-label}}": "{{workflow.parameters.sys-node-pool}}",
			},
			Containers: spec.Containers,
			Volumes: []corev1.Volume{
				{
					Name: "sys-dshm",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium: corev1.StorageMediumMemory,
						},
					},
				},
				{
					Name: "sys-namespace-config",
					VolumeSource: corev1.VolumeSource{
						Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{
								{
									ConfigMap: &corev1.ConfigMapProjection{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: "onepanel",
										},
									},
								},
								{
									Secret: &corev1.SecretProjection{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: "onepanel",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	statefulSet := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "StatefulSet",
		"metadata": metav1.ObjectMeta{
			Name: "{{workflow.parameters.sys-uid}}",
		},
		"spec": map[string]interface{}{
			"replicas":    1,
			"serviceName": "{{workflow.parameters.sys-uid}}",
			"selector": &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "{{workflow.parameters.sys-uid}}",
				},
			},
			"template":             template,
			"volumeClaimTemplates": volumeClaims,
		},
	}
	statefulSetManifestBytes, err := yaml.Marshal(statefulSet)
	if err != nil {
		return
	}
	statefulSetManifest = string(statefulSetManifestBytes)

	return
}

func createWorkspaceManifest(spec *WorkspaceSpec) (workspaceManifest string, err error) {
	// TODO: This needs to be a Kubernetes Go struct
	// TODO: labels should be persisted here as well
	workspace := map[string]interface{}{
		"apiVersion": "onepanel.io/v1alpha1",
		"kind":       "Workspace",
		"metadata": metav1.ObjectMeta{
			Name: "{{workflow.parameters.sys-uid}}",
		},
	}
	workspaceManifestBytes, err := yaml.Marshal(workspace)
	if err != nil {
		return
	}
	workspaceManifest = string(workspaceManifestBytes)

	return
}

func unmarshalWorkflowTemplate(spec *WorkspaceSpec, serviceManifest, virtualServiceManifest, statefulSetManifest, workspaceManifest string) (workflowTemplateSpecManifest string, err error) {
	var volumeClaimItems []wfv1.Item
	volumeClaimsMapped := make(map[string]bool)
	for _, c := range spec.Containers {
		for _, v := range c.VolumeMounts {
			if volumeClaimsMapped[v.Name] {
				continue
			}

			volumeClaimItems = append(volumeClaimItems, wfv1.Item{Type: wfv1.String, StrVal: v.Name})

			volumeClaimsMapped[v.Name] = true
		}
	}

	getStatefulSetManifest := `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: {{workflow.parameters.sys-uid}}
`
	deletePVCManifest := `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: {{inputs.parameters.sys-pvc-name}}-{{workflow.parameters.sys-uid}}-0
`
	templates := []wfv1.Template{
		{
			Name: "workspace",
			DAG: &wfv1.DAGTemplate{
				FailFast: ptr.Bool(false),
				Tasks: []wfv1.DAGTask{
					{
						Name:     "service",
						Template: "service-resource",
					},
					{
						Name:         "virtual-service",
						Template:     "virtual-service-resource",
						Dependencies: []string{"service"},
					},
					{
						Name:         "create-stateful-set",
						Template:     "stateful-set-resource",
						Dependencies: []string{"virtual-service"},
						When:         "{{workflow.parameters.sys-workspace-action}} == create || {{workflow.parameters.sys-workspace-action}} == update",
					},
					{
						Name:         "get-stateful-set",
						Template:     "get-stateful-set-resource",
						Dependencies: []string{"create-stateful-set"},
						When:         "{{workflow.parameters.sys-workspace-action}} == create || {{workflow.parameters.sys-workspace-action}} == update",
						Arguments: wfv1.Arguments{
							Parameters: []wfv1.Parameter{
								{
									Name:  "update-revision",
									Value: ptr.String("{{tasks.create-stateful-set.outputs.parameters.update-revision}}"),
								},
							},
						},
					},
					{
						Name:         "create-workspace",
						Template:     "workspace-resource",
						Dependencies: []string{"get-stateful-set"},
						When:         "{{workflow.parameters.sys-workspace-action}} == create || {{workflow.parameters.sys-workspace-action}} == update",
					},
					{
						Name:         "delete-stateful-set",
						Template:     "delete-stateful-set-resource",
						Dependencies: []string{"virtual-service"},
						When:         "{{workflow.parameters.sys-workspace-action}} == pause || {{workflow.parameters.sys-workspace-action}} == delete",
					},
					{
						Name:         "delete-workspace",
						Template:     "workspace-resource",
						Dependencies: []string{"delete-stateful-set"},
						When:         "{{workflow.parameters.sys-workspace-action}} == pause || {{workflow.parameters.sys-workspace-action}} == delete",
					},
					{
						Name:         "delete-pvc",
						Template:     "delete-pvc-resource",
						Dependencies: []string{"delete-workspace"},
						Arguments: wfv1.Arguments{
							Parameters: []wfv1.Parameter{
								{
									Name:  "sys-pvc-name",
									Value: ptr.String("{{item}}"),
								},
							},
						},
						When:      "{{workflow.parameters.sys-workspace-action}} == delete",
						WithItems: volumeClaimItems,
					},
					{
						Name:         "sys-set-phase-running",
						Template:     "sys-update-status",
						Dependencies: []string{"create-workspace"},
						Arguments: wfv1.Arguments{
							Parameters: []wfv1.Parameter{
								{
									Name:  "sys-workspace-phase",
									Value: ptr.String(string(WorkspaceRunning)),
								},
							},
						},
						When: "{{workflow.parameters.sys-workspace-action}} == create || {{workflow.parameters.sys-workspace-action}} == update",
					},
					{
						Name:         "sys-set-phase-paused",
						Template:     "sys-update-status",
						Dependencies: []string{"delete-workspace"},
						Arguments: wfv1.Arguments{
							Parameters: []wfv1.Parameter{
								{
									Name:  "sys-workspace-phase",
									Value: ptr.String(string(WorkspacePaused)),
								},
							},
						},
						When: "{{workflow.parameters.sys-workspace-action}} == pause",
					},
					{
						Name:         "sys-set-phase-terminated",
						Template:     "sys-update-status",
						Dependencies: []string{"delete-pvc"},
						Arguments: wfv1.Arguments{
							Parameters: []wfv1.Parameter{
								{
									Name:  "sys-workspace-phase",
									Value: ptr.String(string(WorkspaceTerminated)),
								},
							},
						},
						When: "{{workflow.parameters.sys-workspace-action}} == delete",
					},
				},
			},
		},
		{
			Name: "service-resource",
			Resource: &wfv1.ResourceTemplate{
				Action:   "{{workflow.parameters.sys-resource-action}}",
				Manifest: serviceManifest,
			},
		},
		{
			Name: "virtual-service-resource",
			Resource: &wfv1.ResourceTemplate{
				Action:   "{{workflow.parameters.sys-resource-action}}",
				Manifest: virtualServiceManifest,
			},
		},
		{
			Name: "stateful-set-resource",
			Resource: &wfv1.ResourceTemplate{
				Action:           "{{workflow.parameters.sys-resource-action}}",
				Manifest:         statefulSetManifest,
				SuccessCondition: "status.readyReplicas > 0",
			},
			Outputs: wfv1.Outputs{
				Parameters: []wfv1.Parameter{
					{
						Name: "update-revision",
						ValueFrom: &wfv1.ValueFrom{
							JSONPath: "{.status.updateRevision}",
						},
					},
				},
			},
		},
		{
			Name: "get-stateful-set-resource",
			Inputs: wfv1.Inputs{
				Parameters: []wfv1.Parameter{{Name: "update-revision"}},
			},
			Resource: &wfv1.ResourceTemplate{
				Action:           "get",
				Manifest:         getStatefulSetManifest,
				SuccessCondition: "status.readyReplicas > 0, status.currentRevision == {{inputs.parameters.update-revision}}",
			},
		},
		{
			Name: "delete-stateful-set-resource",
			Resource: &wfv1.ResourceTemplate{
				Action:   "{{workflow.parameters.sys-resource-action}}",
				Manifest: statefulSetManifest,
			},
		},
		{
			Name: "workspace-resource",
			Resource: &wfv1.ResourceTemplate{
				Action:   "{{workflow.parameters.sys-resource-action}}",
				Manifest: workspaceManifest,
			},
		},
		{
			Name: "delete-pvc-resource",
			Inputs: wfv1.Inputs{
				Parameters: []wfv1.Parameter{{Name: "sys-pvc-name"}},
			},
			Resource: &wfv1.ResourceTemplate{
				Action:   "{{workflow.parameters.sys-resource-action}}",
				Manifest: deletePVCManifest,
			},
		},
	}
	// Add curl template
	curlPath := fmt.Sprintf("/apis/v1beta1/{{workflow.namespace}}/workspaces/{{workflow.parameters.sys-uid}}/status")
	status := map[string]interface{}{
		"phase": "{{inputs.parameters.sys-workspace-phase}}",
	}
	statusBytes, err := json.Marshal(status)
	if err != nil {
		return
	}
	inputs := wfv1.Inputs{
		Parameters: []wfv1.Parameter{
			{Name: "sys-workspace-phase"},
		},
	}
	curlNodeTemplate, err := getCURLNodeTemplate("sys-update-status", http.MethodPut, curlPath, string(statusBytes), inputs)
	if err != nil {
		return
	}
	templates = append(templates, *curlNodeTemplate)
	// Add postExecutionWorkflow if it exists
	if spec.PostExecutionWorkflow != nil {
		dag := wfv1.DAGTask{
			Name:         spec.PostExecutionWorkflow.Entrypoint,
			Template:     spec.PostExecutionWorkflow.Entrypoint,
			Dependencies: []string{"sys-set-phase-running", "sys-set-phase-paused", "sys-set-phase-terminated"},
		}

		templates[0].DAG.Tasks = append(templates[0].DAG.Tasks, dag)

		templates = append(templates, spec.PostExecutionWorkflow.Templates...)
	}

	workflowTemplateSpec := map[string]interface{}{
		"arguments":  spec.Arguments,
		"entrypoint": "workspace",
		"templates":  templates,
	}

	workflowTemplateSpecManifestBytes, err := yaml.Marshal(workflowTemplateSpec)
	if err != nil {
		return
	}
	workflowTemplateSpecManifest = string(workflowTemplateSpecManifestBytes)

	return
}

func (c *Client) createWorkspaceTemplate(namespace string, workspaceTemplate *WorkspaceTemplate) (*WorkspaceTemplate, error) {
	err := workspaceTemplate.GenerateUID(workspaceTemplate.Name)
	if err != nil {
		return nil, err
	}

	workspaceTemplate.WorkflowTemplate.IsSystem = true
	workspaceTemplate.WorkflowTemplate.Resource = ptr.String(TypeWorkspaceTemplate)
	workspaceTemplate.WorkflowTemplate.ResourceUID = &workspaceTemplate.UID

	// validate workflow template
	if err := c.validateWorkflowTemplate(namespace, workspaceTemplate.WorkflowTemplate); err != nil {
		message := strings.Replace(err.Error(), "{{workflow.", "{{workspace.", -1)
		return nil, util.NewUserError(codes.InvalidArgument, message)
	}
	workspaceTemplate.WorkflowTemplate, _, err = c.createWorkflowTemplate(namespace, workspaceTemplate.WorkflowTemplate)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace":         namespace,
			"WorkspaceTemplate": workspaceTemplate,
			"Error":             err.Error(),
		}).Error("Could not create workflow template for workspace.")
		return nil, util.NewUserErrorWrap(err, "Workflow template")
	}

	workspaceTemplate.Version = workspaceTemplate.WorkflowTemplate.Version
	workspaceTemplate.IsLatest = true

	tx, err := c.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	err = sb.Insert("workspace_templates").
		SetMap(sq.Eq{
			"uid":                  workspaceTemplate.UID,
			"name":                 workspaceTemplate.Name,
			"description":          workspaceTemplate.Description,
			"namespace":            namespace,
			"workflow_template_id": workspaceTemplate.WorkflowTemplate.ID,
			"labels":               workspaceTemplate.Labels,
		}).
		Suffix("RETURNING id, created_at").
		RunWith(tx).
		QueryRow().
		Scan(&workspaceTemplate.ID, &workspaceTemplate.CreatedAt)
	if err != nil {
		_, errCleanUp := c.ArchiveWorkflowTemplate(namespace, workspaceTemplate.WorkflowTemplate.UID)
		errorMsg := "Error with insert into workspace_templates. "
		if errCleanUp != nil {
			errorMsg += "Error with clean-up: ArchiveWorkflowTemplate. "
			errorMsg += errCleanUp.Error()
		}
		return nil, util.NewUserErrorWrap(err, errorMsg) //return the source error
	}

	err = createWorkspaceTemplateVersionDB(tx, workspaceTemplate)
	if err != nil {
		errorMsg := "Error with insert into workspace_templates_versions. "
		_, errCleanUp := c.ArchiveWorkflowTemplate(namespace, workspaceTemplate.WorkflowTemplate.UID)
		if errCleanUp != nil {
			err = fmt.Errorf("%w; %s", err, errCleanUp)
			errorMsg += "Error with clean-up: ArchiveWorkflowTemplate. "
		}
		return nil, util.NewUserErrorWrap(err, errorMsg) // return the source error
	}

	if err = tx.Commit(); err != nil {
		_, errArchive := c.ArchiveWorkflowTemplate(namespace, workspaceTemplate.WorkflowTemplate.UID)
		if errArchive != nil {
			err = fmt.Errorf("%w; %s", err, errArchive)
		}
		return nil, err
	}

	return workspaceTemplate, nil
}

func (c *Client) workspaceTemplatesSelectBuilder(namespace string) sq.SelectBuilder {
	sb := sb.Select(getWorkspaceTemplateColumns("wt")...).
		From("workspace_templates wt").
		Where(sq.Eq{
			"wt.namespace": namespace,
		})

	return sb
}

func (c *Client) workspaceTemplateVersionsSelectBuilder(namespace, uid string) sq.SelectBuilder {
	sb := sb.Select(getWorkspaceTemplateColumnsWithoutLabels("wt")...).
		From("workspace_templates wt").
		Columns("wtv.id \"workspace_template_version_id\"", "wtv.created_at \"created_at\"", "wtv.version", "wtv.manifest", "wtv.labels", "wft.id \"workflow_template.id\"", "wft.uid \"workflow_template.uid\"", "wftv.version \"workflow_template.version\"", "wftv.manifest \"workflow_template.manifest\"").
		Join("workspace_template_versions wtv ON wtv.workspace_template_id = wt.id").
		Join("workflow_templates wft ON wft.id = wt.workflow_template_id").
		Join("workflow_template_versions wftv ON wftv.workflow_template_id = wft.id").
		Where(sq.Eq{
			"wt.uid":       uid,
			"wt.namespace": namespace,
		})

	return sb
}

func (c *Client) getWorkspaceTemplateByName(namespace, name string) (workspaceTemplate *WorkspaceTemplate, err error) {
	workspaceTemplate = &WorkspaceTemplate{}

	sb := c.workspaceTemplatesSelectBuilder(namespace).
		Where(sq.Eq{
			"wt.name":     name,
			"is_archived": false,
		}).
		Limit(1)
	query, args, err := sb.ToSql()
	if err != nil {
		return
	}

	if err = c.DB.Get(workspaceTemplate, query, args...); err == sql.ErrNoRows {
		err = nil
		workspaceTemplate = nil
	}

	return
}

func (c *Client) generateWorkspaceTemplateWorkflowTemplate(workspaceTemplate *WorkspaceTemplate) (workflowTemplate *WorkflowTemplate, err error) {
	if workspaceTemplate == nil || workspaceTemplate.Manifest == "" {
		return nil, util.NewUserError(codes.InvalidArgument, "Workspace template manifest is required")
	}

	config, err := c.GetSystemConfig()
	if err != nil {
		return nil, err
	}

	workspaceSpec, err := parseWorkspaceSpec(workspaceTemplate.Manifest)
	if err != nil {
		return nil, util.NewUserError(codes.InvalidArgument, err.Error())
	}

	if err = generateArguments(workspaceSpec, config); err != nil {
		return nil, err
	}

	serviceManifest, err := createServiceManifest(workspaceSpec)
	if err != nil {
		return nil, err
	}

	virtualServiceManifest, err := createVirtualServiceManifest(workspaceSpec)
	if err != nil {
		return nil, err
	}

	services, err := c.ListServices(workspaceTemplate.Namespace)
	if err != nil {
		return nil, err
	}

	statefulSetManifest, err := createStatefulSetManifest(workspaceSpec, config, services)
	if err != nil {
		return nil, err
	}

	workspaceManifest, err := createWorkspaceManifest(workspaceSpec)
	if err != nil {
		return nil, err
	}

	workflowTemplateManifest, err := unmarshalWorkflowTemplate(workspaceSpec, serviceManifest, virtualServiceManifest, statefulSetManifest, workspaceManifest)
	if err != nil {
		return nil, err
	}

	workflowTemplateManifest = strings.NewReplacer(
		"{{workspace.parameters.", "{{workflow.parameters.").Replace(workflowTemplateManifest)

	workflowTemplate = &WorkflowTemplate{
		Name:     workspaceTemplate.Name,
		Manifest: workflowTemplateManifest,
	}

	return workflowTemplate, nil
}

// CreateWorkspaceTemplateWorkflowTemplate generates and returns a workflowTemplate for a given workspaceTemplate manifest
func (c *Client) GenerateWorkspaceTemplateWorkflowTemplate(workspaceTemplate *WorkspaceTemplate) (workflowTemplate *WorkflowTemplate, err error) {
	workflowTemplate, err = c.generateWorkspaceTemplateWorkflowTemplate(workspaceTemplate)
	if err != nil {
		return nil, err
	}

	return workflowTemplate, nil
}

// CreateWorkspaceTemplate creates a template for Workspaces
func (c *Client) CreateWorkspaceTemplate(namespace string, workspaceTemplate *WorkspaceTemplate) (*WorkspaceTemplate, error) {
	_, err := govalidator.ValidateStruct(workspaceTemplate)
	if err != nil {
		return nil, util.NewUserError(codes.InvalidArgument, err.Error())
	}
	workspaceTemplate.Namespace = namespace

	existingWorkspaceTemplate, err := c.getWorkspaceTemplateByName(namespace, workspaceTemplate.Name)
	if err != nil {
		return nil, err
	}

	if existingWorkspaceTemplate != nil {
		message := fmt.Sprintf("Workspace template with the name '%v' already exists", workspaceTemplate.Name)
		if existingWorkspaceTemplate.IsArchived {
			message = fmt.Sprintf("An archived workspace template with the name '%v' already exists", workspaceTemplate.Name)
		}
		return nil, util.NewUserError(codes.AlreadyExists, message)
	}

	workspaceTemplate.WorkflowTemplate, err = c.generateWorkspaceTemplateWorkflowTemplate(workspaceTemplate)
	if err != nil {
		return nil, err
	}

	workspaceTemplate, err = c.createWorkspaceTemplate(namespace, workspaceTemplate)
	if err != nil {
		var statusError *util.UserError
		if goerrors.As(err, &statusError) && statusError.Code == codes.InvalidArgument {
			return nil, util.NewUserError(statusError.Code, strings.Replace(statusError.Message, "{{workflow.", "{{workspace.", -1))
		}
		return nil, err
	}

	return workspaceTemplate, nil
}

// GetWorkspaceTemplate return a workspaceTemplate and its corresponding workflowTemplate
// if version is 0, the latest version is returned.
func (c *Client) GetWorkspaceTemplate(namespace, uid string, version int64) (workspaceTemplate *WorkspaceTemplate, err error) {
	sb := c.workspaceTemplateVersionsSelectBuilder(namespace, uid).
		Limit(1)

	sb = sb.Where(sq.Eq{"wt.is_archived": false})

	if version == 0 {
		sb = sb.Where(sq.Eq{
			"wtv.is_latest":  true,
			"wftv.is_latest": true,
		})
	} else {
		sb = sb.Where(sq.Eq{
			"wtv.version":  version,
			"wftv.version": version,
		})
	}

	workspaceTemplate = &WorkspaceTemplate{}
	if err = c.DB.Getx(workspaceTemplate, sb); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}

		return
	}

	sysConfig, err := c.GetSystemConfig()
	if err != nil {
		return nil, err
	}

	if err := workspaceTemplate.InjectRuntimeParameters(sysConfig); err != nil {
		return nil, err
	}

	return
}

// UpdateWorkspaceTemplate adds a new workspace template version
func (c *Client) UpdateWorkspaceTemplate(namespace string, workspaceTemplate *WorkspaceTemplate) (*WorkspaceTemplate, error) {
	existingWorkspaceTemplate, err := c.GetWorkspaceTemplate(namespace, workspaceTemplate.UID, workspaceTemplate.Version)
	if err != nil {
		return nil, err
	}
	if existingWorkspaceTemplate == nil {
		return nil, util.NewUserError(codes.NotFound, "Workspace template not found.")
	}
	workspaceTemplate.ID = existingWorkspaceTemplate.ID
	workspaceTemplate.Name = existingWorkspaceTemplate.UID
	workspaceTemplate.Namespace = existingWorkspaceTemplate.Namespace

	updatedWorkflowTemplate, err := c.generateWorkspaceTemplateWorkflowTemplate(workspaceTemplate)
	if err != nil {
		return nil, err
	}
	updatedWorkflowTemplate.ID = existingWorkspaceTemplate.WorkflowTemplate.ID
	updatedWorkflowTemplate.UID = existingWorkspaceTemplate.WorkflowTemplate.UID

	updatedWorkflowTemplate.Labels = workspaceTemplate.Labels
	workflowTemplateVersion, err := c.CreateWorkflowTemplateVersion(namespace, updatedWorkflowTemplate)
	if err != nil {
		var statusError *util.UserError
		if goerrors.As(err, &statusError) && statusError.Code == codes.InvalidArgument {
			return nil, util.NewUserError(statusError.Code, strings.Replace(statusError.Message, "{{workflow.", "{{workspace.", -1))
		}
		return nil, err
	}

	// TODO - this might not be needed with recent changes made.
	workspaceTemplate.Version = workflowTemplateVersion.Version
	workspaceTemplate.IsLatest = true

	tx, err := c.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := createLatestWorkspaceTemplateVersionDB(tx, workspaceTemplate); err != nil {
		return nil, err
	}

	_, err = sb.Update("workspace_templates").
		SetMap(sq.Eq{
			"labels":      workspaceTemplate.Labels,
			"description": workspaceTemplate.Description,
		}).
		Where(sq.Eq{
			"uid":       workspaceTemplate.UID,
			"namespace": workspaceTemplate.Namespace,
		}).
		RunWith(tx).
		Exec()
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return workspaceTemplate, nil
}

// ListWorkspaceTemplates returns a list of workspace templates that are not archived, sorted by most recent created first
func (c *Client) ListWorkspaceTemplates(namespace string, paginator *pagination.PaginationRequest) (workspaceTemplates []*WorkspaceTemplate, err error) {
	sb := c.workspaceTemplatesSelectBuilder(namespace).
		Where(sq.Eq{
			"wt.is_archived": false,
		}).
		OrderBy("wt.created_at DESC")

	sb = *paginator.ApplyToSelect(&sb)

	err = c.DB.Selectx(&workspaceTemplates, sb)

	return
}

// ListWorkspaceTemplateVersions returns an array of WorkspaceTemplates with the version information loaded. Latest id is first.
func (c *Client) ListWorkspaceTemplateVersions(namespace, uid string) (workspaceTemplates []*WorkspaceTemplate, err error) {
	sb := c.workspaceTemplateVersionsSelectBuilder(namespace, uid).
		Options("DISTINCT ON (wtv.version) wtv.version,").
		Where(sq.Eq{
			"wt.is_archived":  false,
			"wft.is_archived": false,
		}).
		OrderBy("wtv.version DESC")

	err = c.DB.Selectx(&workspaceTemplates, sb)

	return
}

// CountWorkspaceTemplates returns the total number of non-archived workspace templates for the input namespace
func (c *Client) CountWorkspaceTemplates(namespace string) (count int, err error) {
	err = sb.Select("count(*)").
		From("workspace_templates wt").
		Where(sq.Eq{
			"wt.namespace":   namespace,
			"wt.is_archived": false,
		}).
		RunWith(c.DB).
		QueryRow().
		Scan(&count)

	return
}

// archiveWorkspaceTemplateDB marks the Workspace template identified by (namespace, uid) and is_archived=false, as archived.
//
// This method returns (true, nil) when the database record was successfully archived.
// If there was no record to archive, (false, nil) is returned.
func (c *Client) archiveWorkspaceTemplateDB(namespace, uid string) (archived bool, err error) {
	result, err := sb.Update("workspace_templates").
		Set("is_archived", true).
		Where(sq.Eq{
			"uid":         uid,
			"namespace":   namespace,
			"is_archived": false,
		}).
		RunWith(c.DB).
		Exec()
	if err != nil {
		return false, err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	if rowsAffected == 0 {
		return false, nil
	}

	return true, nil
}

// WorkspaceTemplateHasRunningWorkspaces returns true if there are non-terminated (or terminating) workspaces that are
// based of this template. False otherwise.
func (c *Client) WorkspaceTemplateHasRunningWorkspaces(namespace string, uid string) (bool, error) {
	runningCount := 0

	err := sb.Select("COUNT(*)").
		From("workspaces w").
		Join("workspace_templates wt ON wt.id = w.workspace_template_id").
		Where(sq.And{
			sq.Eq{
				"wt.namespace": namespace,
				"wt.uid":       uid,
			}, sq.NotEq{
				"w.phase": []string{"Terminated"},
			}}).
		RunWith(c.DB).
		QueryRow().
		Scan(&runningCount)
	if err != nil {
		return false, err
	}

	return runningCount > 0, nil
}

// ArchiveWorkspaceTemplate archives and deletes resources associated with the workspace template.
//
// In particular, this action
//
// * Archives each workspace (k8s cleaned-up, database entry marked archived)
//
// * Marks associated Workflow template as archived
//
// * Marks associated Workflow executions as archived
//
// * Deletes Workflow Executions in k8s
func (c *Client) ArchiveWorkspaceTemplate(namespace string, uid string) (archived bool, err error) {
	wsTemp, err := c.GetWorkspaceTemplate(namespace, uid, 0)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"UID":       uid,
			"Error":     err.Error(),
		}).Error("Get Workspace Template failed.")
		return false, util.NewUserError(codes.Unknown, "Unable to archive workspace template.")
	}
	if wsTemp == nil {
		return false, fmt.Errorf("not found")
	}

	wsList, err := c.ListWorkspacesByTemplateID(namespace, wsTemp.WorkspaceTemplateVersionID)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"UID":       uid,
			"Error":     err.Error(),
		}).Error("ListWorkspacesByTemplateID failed.")
		return false, util.NewUserError(codes.Unknown, "Unable to archive workspace template.")
	}

	for _, ws := range wsList {
		err = c.ArchiveWorkspace(namespace, ws.UID)
		if err != nil {
			log.WithFields(log.Fields{
				"Namespace": namespace,
				"UID":       uid,
				"Error":     err.Error(),
			}).Error("ArchiveWorkspace failed.")
			return false, util.NewUserError(codes.Unknown, "Unable to archive workspace template.")
		}
	}

	_, err = c.archiveWorkspaceTemplateDB(namespace, wsTemp.UID)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"UID":       uid,
			"Error":     err.Error(),
		}).Error("Archive Workspace Template DB Failed.")
		return false, util.NewUserError(codes.Unknown, "Unable to archive workspace template.")
	}

	_, err = c.ArchiveWorkflowTemplate(namespace, wsTemp.UID)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"UID":       uid,
			"Error":     err.Error(),
		}).Error("Archive Workflow Template Failed.")
		return false, util.NewUserError(codes.Unknown, "Unable to archive workspace template.")
	}

	return true, nil
}
