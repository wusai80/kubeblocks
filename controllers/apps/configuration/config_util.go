/*
Copyright (C) 2022-2024 ApeCloud Co., Ltd

This file is part of KubeBlocks project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package configuration

import (
	"context"
	"fmt"
	"reflect"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/conversion"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1alpha1 "github.com/apecloud/kubeblocks/apis/apps/v1alpha1"
	cfgcm "github.com/apecloud/kubeblocks/pkg/configuration/config_manager"
	"github.com/apecloud/kubeblocks/pkg/configuration/core"
	"github.com/apecloud/kubeblocks/pkg/configuration/openapi"
	"github.com/apecloud/kubeblocks/pkg/configuration/validate"
	"github.com/apecloud/kubeblocks/pkg/constant"
	intctrlutil "github.com/apecloud/kubeblocks/pkg/controllerutil"
	"github.com/apecloud/kubeblocks/pkg/generics"
)

type ValidateConfigMap func(configTpl, ns string) (*corev1.ConfigMap, error)
type ValidateConfigSchema func(tpl *appsv1alpha1.CustomParametersValidation) (bool, error)

func checkConfigLabels(object client.Object, requiredLabs []string) bool {
	labels := object.GetLabels()
	if len(labels) == 0 {
		return false
	}

	for _, label := range requiredLabs {
		if _, ok := labels[label]; !ok {
			return false
		}
	}

	// reconfigure ConfigMap for db instance
	if ins, ok := labels[constant.CMConfigurationTypeLabelKey]; !ok || ins != constant.ConfigInstanceType {
		return false
	}

	return checkEnableCfgUpgrade(object)
}

func getConfigMapByTemplateName(cli client.Client, ctx intctrlutil.RequestCtx, templateName, ns string) (*corev1.ConfigMap, error) {
	if len(templateName) == 0 {
		return nil, fmt.Errorf("required configmap reference name is empty! [%v]", templateName)
	}

	configObj := &corev1.ConfigMap{}
	if err := cli.Get(ctx.Ctx, client.ObjectKey{
		Namespace: ns,
		Name:      templateName,
	}, configObj); err != nil {
		ctx.Log.Error(err, "failed to get config template cm object!", "configMapName", templateName)
		return nil, err
	}

	return configObj, nil
}

func checkConfigConstraint(ctx intctrlutil.RequestCtx, configConstraint *appsv1alpha1.ConfigConstraint) (bool, error) {
	// validate configuration template
	validateConfigSchema := func(ccSchema *appsv1alpha1.CustomParametersValidation) (bool, error) {
		if ccSchema == nil || len(ccSchema.CUE) == 0 {
			return true, nil
		}

		err := validate.CueValidate(ccSchema.CUE)
		return err == nil, err
	}

	// validate schema
	if ok, err := validateConfigSchema(configConstraint.Spec.ConfigurationSchema); !ok || err != nil {
		ctx.Log.Error(err, "failed to validate template schema!", "configMapName", fmt.Sprintf("%v", configConstraint.Spec.ConfigurationSchema))
		return ok, err
	}
	return true, nil
}

func ReconcileConfigSpecsForReferencedCR[T generics.Object, PT generics.PObject[T]](client client.Client, ctx intctrlutil.RequestCtx, obj PT) error {
	if ok, err := checkConfigTemplate(client, ctx, obj); !ok || err != nil {
		return fmt.Errorf("failed to check config template: %v", err)
	}
	if ok, err := updateLabelsByConfigSpec(client, ctx, obj); !ok || err != nil {
		return fmt.Errorf("failed to update using config template info: %v", err)
	}
	if _, err := updateConfigMapFinalizer(client, ctx, obj); err != nil {
		return fmt.Errorf("failed to update config map finalizer: %v", err)
	}
	return nil
}

func DeleteConfigMapFinalizer(cli client.Client, ctx intctrlutil.RequestCtx, obj client.Object) error {
	handler := func(configSpecs []appsv1alpha1.ComponentConfigSpec) (bool, error) {
		return true, batchDeleteConfigMapFinalizer(cli, ctx, configSpecs, obj)
	}
	_, err := handleConfigTemplate(obj, handler)
	return err
}

func validateConfigMapOwners(cli client.Client, ctx intctrlutil.RequestCtx, labels client.MatchingLabels, check func(obj client.Object) bool, objLists ...client.ObjectList) (bool, error) {
	for _, objList := range objLists {
		if err := cli.List(ctx.Ctx, objList, labels, client.Limit(2)); err != nil {
			return false, err
		}
		v, err := conversion.EnforcePtr(objList)
		if err != nil {
			return false, err
		}
		items := v.FieldByName("Items")
		if !items.IsValid() || items.Kind() != reflect.Slice || items.Len() > 1 {
			return false, nil
		}
		if items.Len() == 0 {
			continue
		}

		val := items.Index(0)
		// fetch object pointer
		if val.CanAddr() {
			val = val.Addr()
		}
		if !val.CanInterface() || !check(val.Interface().(client.Object)) {
			return false, nil
		}
	}
	return true, nil
}

func batchDeleteConfigMapFinalizer(cli client.Client, ctx intctrlutil.RequestCtx, configSpecs []appsv1alpha1.ComponentConfigSpec, cr client.Object) error {
	validator := func(obj client.Object) bool {
		return obj.GetName() == cr.GetName() && obj.GetNamespace() == cr.GetNamespace()
	}
	for _, configSpec := range configSpecs {
		labels := client.MatchingLabels{
			core.GenerateTPLUniqLabelKeyWithConfig(configSpec.Name): configSpec.TemplateRef,
		}
		if ok, err := validateConfigMapOwners(cli, ctx, labels, validator, &appsv1alpha1.ClusterVersionList{}, &appsv1alpha1.ClusterDefinitionList{}); err != nil {
			return err
		} else if !ok {
			continue
		}
		if err := deleteConfigMapFinalizer(cli, ctx, configSpec); err != nil {
			return err
		}
	}
	return nil
}

func updateConfigMapFinalizer(client client.Client, ctx intctrlutil.RequestCtx, obj client.Object) (bool, error) {
	handler := func(configSpecs []appsv1alpha1.ComponentConfigSpec) (bool, error) {
		return true, batchUpdateConfigMapFinalizer(client, ctx, configSpecs)
	}
	return handleConfigTemplate(obj, handler)
}

func batchUpdateConfigMapFinalizer(cli client.Client, ctx intctrlutil.RequestCtx, configSpecs []appsv1alpha1.ComponentConfigSpec) error {
	for _, configSpec := range configSpecs {
		if err := updateConfigMapFinalizerImpl(cli, ctx, configSpec); err != nil {
			return err
		}
	}
	return nil
}

func updateConfigMapFinalizerImpl(cli client.Client, ctx intctrlutil.RequestCtx, configSpec appsv1alpha1.ComponentConfigSpec) error {
	// step1: add finalizer
	// step2: add labels: CMConfigurationTypeLabelKey
	// step3: update immutable

	cmObj, err := getConfigMapByTemplateName(cli, ctx, configSpec.TemplateRef, configSpec.Namespace)
	if err != nil {
		ctx.Log.Error(err, "failed to get template cm object!", "configMapName", cmObj.Name)
		return err
	}

	if controllerutil.ContainsFinalizer(cmObj, constant.ConfigurationTemplateFinalizerName) {
		return nil
	}

	patch := client.MergeFrom(cmObj.DeepCopy())

	if cmObj.Labels == nil {
		cmObj.Labels = map[string]string{}
	}
	cmObj.Labels[constant.CMConfigurationTypeLabelKey] = constant.ConfigTemplateType
	controllerutil.AddFinalizer(cmObj, constant.ConfigurationTemplateFinalizerName)

	// cmObj.Immutable = &tpl.Spec.Immutable
	return cli.Patch(ctx.Ctx, cmObj, patch)
}

func deleteConfigMapFinalizer(cli client.Client, ctx intctrlutil.RequestCtx, configSpec appsv1alpha1.ComponentConfigSpec) error {
	cmObj, err := getConfigMapByTemplateName(cli, ctx, configSpec.TemplateRef, configSpec.Namespace)
	if err != nil && apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		ctx.Log.Error(err, "failed to get config template cm object!", "configMapName", configSpec.TemplateRef)
		return err
	}

	if !controllerutil.ContainsFinalizer(cmObj, constant.ConfigurationTemplateFinalizerName) {
		return nil
	}

	patch := client.MergeFrom(cmObj.DeepCopy())
	controllerutil.RemoveFinalizer(cmObj, constant.ConfigurationTemplateFinalizerName)
	return cli.Patch(ctx.Ctx, cmObj, patch)
}

type ConfigTemplateHandler func([]appsv1alpha1.ComponentConfigSpec) (bool, error)
type ComponentValidateHandler func(component *appsv1alpha1.ClusterComponentDefinition) error

func handleConfigTemplate(object client.Object, handler ConfigTemplateHandler, handler2 ...ComponentValidateHandler) (bool, error) {
	var (
		err             error
		configTemplates []appsv1alpha1.ComponentConfigSpec
	)
	switch cr := object.(type) {
	case *appsv1alpha1.ClusterDefinition:
		configTemplates, err = getConfigTemplateFromCD(cr, handler2...)
	case *appsv1alpha1.ClusterVersion:
		configTemplates = getConfigTemplateFromCV(cr)
	default:
		return false, core.MakeError("not support CR type: %v", cr)
	}

	switch {
	case err != nil:
		return false, err
	case len(configTemplates) > 0:
		return handler(configTemplates)
	default:
		return true, nil
	}
}

func getConfigTemplateFromCV(appVer *appsv1alpha1.ClusterVersion) []appsv1alpha1.ComponentConfigSpec {
	configTemplates := make([]appsv1alpha1.ComponentConfigSpec, 0)
	for _, component := range appVer.Spec.ComponentVersions {
		if len(component.ConfigSpecs) > 0 {
			configTemplates = append(configTemplates, component.ConfigSpecs...)
		}
	}
	return configTemplates
}

func getConfigTemplateFromCD(clusterDef *appsv1alpha1.ClusterDefinition, validators ...ComponentValidateHandler) ([]appsv1alpha1.ComponentConfigSpec, error) {
	configTemplates := make([]appsv1alpha1.ComponentConfigSpec, 0)
	for _, component := range clusterDef.Spec.ComponentDefs {
		// For compatibility with the previous lifecycle management of configurationSpec.TemplateRef, it is necessary to convert ScriptSpecs to ConfigSpecs,
		// ensuring that the script-related configmap is not allowed to be deleted.
		for _, scriptSpec := range component.ScriptSpecs {
			configTemplates = append(configTemplates, appsv1alpha1.ComponentConfigSpec{
				ComponentTemplateSpec: scriptSpec,
			})
		}
		if len(component.ConfigSpecs) == 0 {
			continue
		}
		configTemplates = append(configTemplates, component.ConfigSpecs...)
		// Check reload configure config template
		for _, validator := range validators {
			if err := validator(&component); err != nil {
				return nil, err
			}
		}
	}
	return configTemplates, nil
}

func checkConfigTemplate(client client.Client, ctx intctrlutil.RequestCtx, obj client.Object) (bool, error) {
	handler := func(configSpecs []appsv1alpha1.ComponentConfigSpec) (bool, error) {
		return validateConfigTemplate(client, ctx, configSpecs)
	}
	return handleConfigTemplate(obj, handler)
}

func updateLabelsByConfigSpec[T generics.Object, PT generics.PObject[T]](cli client.Client, ctx intctrlutil.RequestCtx, obj PT) (bool, error) {
	handler := func(configSpecs []appsv1alpha1.ComponentConfigSpec) (bool, error) {
		patch := client.MergeFrom(PT(obj.DeepCopy()))
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		for _, configSpec := range configSpecs {
			labels[core.GenerateTPLUniqLabelKeyWithConfig(configSpec.Name)] = configSpec.TemplateRef
			if len(configSpec.ConfigConstraintRef) != 0 {
				labels[core.GenerateConstraintsUniqLabelKeyWithConfig(configSpec.ConfigConstraintRef)] = configSpec.ConfigConstraintRef
			}
		}
		obj.SetLabels(labels)
		return true, cli.Patch(ctx.Ctx, obj, patch)
	}
	return handleConfigTemplate(obj, handler)
}

func validateConfigTemplate(cli client.Client, ctx intctrlutil.RequestCtx, configSpecs []appsv1alpha1.ComponentConfigSpec) (bool, error) {
	// validate ConfigTemplate
	foundAndCheckConfigSpec := func(configSpec appsv1alpha1.ComponentConfigSpec, logger logr.Logger) (*appsv1alpha1.ConfigConstraint, error) {
		if _, err := getConfigMapByTemplateName(cli, ctx, configSpec.TemplateRef, configSpec.Namespace); err != nil {
			logger.Error(err, "failed to get config template cm object!")
			return nil, err
		}
		if configSpec.VolumeName == "" && len(configSpec.AsEnvFrom) == 0 {
			return nil, core.MakeError("config template volume name and envFrom is empty!")
		}
		if configSpec.ConfigConstraintRef == "" {
			return nil, nil
		}
		configKey := client.ObjectKey{
			Namespace: "",
			Name:      configSpec.ConfigConstraintRef,
		}
		configObj := &appsv1alpha1.ConfigConstraint{}
		if err := cli.Get(ctx.Ctx, configKey, configObj); err != nil {
			logger.Error(err, "failed to get template cm object!")
			return nil, err
		}
		return configObj, nil
	}

	for _, templateRef := range configSpecs {
		logger := ctx.Log.WithValues("templateName", templateRef.Name).WithValues("configMapName", templateRef.TemplateRef)
		configConstraint, err := foundAndCheckConfigSpec(templateRef, logger)
		if err != nil {
			logger.Error(err, "failed to validate config template!")
			return false, err
		}
		if configConstraint == nil || configConstraint.Spec.ReloadOptions == nil {
			continue
		}
		if err := cfgcm.ValidateReloadOptions(configConstraint.Spec.ReloadOptions, cli, ctx.Ctx); err != nil {
			return false, err
		}
		if !validateConfigConstraintStatus(configConstraint.Status) {
			errMsg := fmt.Sprintf("Configuration template CR[%s] status not ready! current status: %s", configConstraint.Name, configConstraint.Status.Phase)
			logger.V(1).Info(errMsg)
			return false, fmt.Errorf(errMsg)
		}
	}
	return true, nil
}

func validateConfigConstraintStatus(ccStatus appsv1alpha1.ConfigConstraintStatus) bool {
	return ccStatus.Phase == appsv1alpha1.CCAvailablePhase
}

func updateConfigConstraintStatus(cli client.Client, ctx intctrlutil.RequestCtx, configConstraint *appsv1alpha1.ConfigConstraint, phase appsv1alpha1.ConfigConstraintPhase) error {
	patch := client.MergeFrom(configConstraint.DeepCopy())
	configConstraint.Status.Phase = phase
	configConstraint.Status.ObservedGeneration = configConstraint.Generation
	return cli.Status().Patch(ctx.Ctx, configConstraint, patch)
}

func createConfigPatch(cfg *corev1.ConfigMap, formatter *appsv1alpha1.FormatterConfig, cmKeys []string) (*core.ConfigPatchInfo, bool, error) {
	// support full update
	if formatter == nil {
		return nil, true, nil
	}
	lastConfig, err := getLastVersionConfig(cfg)
	if err != nil {
		return nil, false, core.WrapError(err, "failed to get last version data. config[%v]", client.ObjectKeyFromObject(cfg))
	}

	return core.CreateConfigPatch(lastConfig, cfg.Data, formatter.Format, cmKeys, true)
}

func updateConfigSchema(cc *appsv1alpha1.ConfigConstraint, cli client.Client, ctx context.Context) error {
	schema := cc.Spec.ConfigurationSchema
	if schema == nil || schema.CUE == "" {
		return nil
	}

	// Because the conversion of cue to openAPISchema is restricted, and the definition of some cue may not be converted into openAPISchema, and won't return error.
	openAPISchema, err := openapi.GenerateOpenAPISchema(schema.CUE, cc.Spec.CfgSchemaTopLevelName)
	if err != nil {
		return err
	}
	if openAPISchema == nil {
		return nil
	}
	if reflect.DeepEqual(openAPISchema, schema.Schema) {
		return nil
	}

	ccPatch := client.MergeFrom(cc.DeepCopy())
	cc.Spec.ConfigurationSchema.Schema = openAPISchema
	return cli.Patch(ctx, cc, ccPatch)
}
