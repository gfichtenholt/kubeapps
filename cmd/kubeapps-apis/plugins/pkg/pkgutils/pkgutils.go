// Copyright 2021-2022 the Kubeapps contributors.
// SPDX-License-Identifier: Apache-2.0

/*
 Utility functions that apply to "packages", e.g. helm charts or carvel packages
*/
package pkgutils

import (
	"bufio"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	"github.com/Masterminds/semver/v3"
	corev1 "github.com/kubeapps/kubeapps/cmd/kubeapps-apis/gen/core/packages/v1alpha1"
	plugins "github.com/kubeapps/kubeapps/cmd/kubeapps-apis/gen/core/plugins/v1alpha1"
	"github.com/kubeapps/kubeapps/pkg/chart/models"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3" // The usual "sigs.k8s.io/yaml" doesn't work: https://github.com/kubeapps/kubeapps/pull/4050
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apimachinery/pkg/runtime"
)

// Contains miscellaneous package-utilities used by multiple plug-ins
const (
	MajorVersionsInSummary = 3
	MinorVersionsInSummary = 3
	PatchVersionsInSummary = 3
)

// Wrapper struct to include three version constants
type VersionsInSummary struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
	Patch int `json:"patch"`
}

var (
	defaultVersionsInSummary = VersionsInSummary{
		Major: MajorVersionsInSummary,
		Minor: MinorVersionsInSummary,
		Patch: PatchVersionsInSummary,
	}
)

func GetDefaultVersionsInSummary() VersionsInSummary {
	return defaultVersionsInSummary
}

// packageAppVersionsSummary converts the model chart versions into the required version summary.
func PackageAppVersionsSummary(versions []models.ChartVersion, versionInSummary VersionsInSummary) []*corev1.PackageAppVersion {
	pav := []*corev1.PackageAppVersion{}

	// Use a version map to be able to count how many major, minor and patch versions
	// we have included.
	version_map := map[uint64]map[uint64][]uint64{}
	for _, v := range versions {
		version, err := semver.NewVersion(v.Version)
		if err != nil {
			continue
		}

		if _, ok := version_map[version.Major()]; !ok {
			// Don't add a new major version if we already have enough
			if len(version_map) >= versionInSummary.Major {
				continue
			}
		} else {
			// If we don't yet have this minor version
			if _, ok := version_map[version.Major()][version.Minor()]; !ok {
				// Don't add a new minor version if we already have enough for this major version
				if len(version_map[version.Major()]) >= versionInSummary.Minor {
					continue
				}
			} else {
				if len(version_map[version.Major()][version.Minor()]) >= versionInSummary.Patch {
					continue
				}
			}
		}

		// Include the version and update the version map.
		pav = append(pav, &corev1.PackageAppVersion{
			PkgVersion: v.Version,
			AppVersion: v.AppVersion,
		})

		if _, ok := version_map[version.Major()]; !ok {
			version_map[version.Major()] = map[uint64][]uint64{}
		}
		version_map[version.Major()][version.Minor()] = append(version_map[version.Major()][version.Minor()], version.Patch())
	}

	return pav
}

// isValidChart returns true if the chart model passed defines a value
// for each required field described at the Helm website:
// https://helm.sh/docs/topics/charts/#the-chartyaml-file
// together with required fields for our model.
func IsValidChart(chart *models.Chart) (bool, error) {
	if chart.Name == "" {
		return false, status.Errorf(codes.Internal, "required field .Name not found on helm chart: %v", chart)
	}
	if chart.ID == "" {
		return false, status.Errorf(codes.Internal, "required field .ID not found on helm chart: %v", chart)
	}
	if chart.Repo == nil {
		return false, status.Errorf(codes.Internal, "required field .Repo not found on helm chart: %v", chart)
	}
	if chart.ChartVersions == nil || len(chart.ChartVersions) == 0 {
		return false, status.Errorf(codes.Internal, "required field .chart.ChartVersions[0] not found on helm chart: %v", chart)
	} else {
		for _, chartVersion := range chart.ChartVersions {
			if chartVersion.Version == "" {
				return false, status.Errorf(codes.Internal, "required field .ChartVersions[i].Version not found on helm chart: %v", chart)
			}
		}
	}
	for _, maintainer := range chart.Maintainers {
		if maintainer.Name == "" {
			return false, status.Errorf(codes.Internal, "required field .Maintainers[i].Name not found on helm chart: %v", chart)
		}
	}
	return true, nil
}

// AvailablePackageSummaryFromChart builds an AvailablePackageSummary from a Chart
func AvailablePackageSummaryFromChart(chart *models.Chart, plugin *plugins.Plugin) (*corev1.AvailablePackageSummary, error) {
	pkg := &corev1.AvailablePackageSummary{}

	isValid, err := IsValidChart(chart)
	if !isValid || err != nil {
		return nil, status.Errorf(codes.Internal, "invalid chart: %s", err.Error())
	}

	pkg.Name = chart.Name
	// Helm's Chart.yaml (and hence our model) does not include a separate
	// display name, so the chart name is also used here.
	pkg.DisplayName = chart.Name
	pkg.IconUrl = chart.Icon
	pkg.ShortDescription = chart.Description
	// TODO (gfichtenholt) I think when chart.Category is "" (i.e. missing)
	// we should return nil rather than []string{""}. For now keeping the logic
	// as is, so as not to break existing unit tests
	pkg.Categories = []string{chart.Category}

	pkg.AvailablePackageRef = &corev1.AvailablePackageReference{
		Identifier: chart.ID,
		Plugin:     plugin,
	}
	pkg.AvailablePackageRef.Context = &corev1.Context{Namespace: chart.Repo.Namespace}

	if chart.ChartVersions != nil || len(chart.ChartVersions) != 0 {
		pkg.LatestVersion = &corev1.PackageAppVersion{
			PkgVersion: chart.ChartVersions[0].Version,
			AppVersion: chart.ChartVersions[0].AppVersion,
		}
	}

	return pkg, nil
}

// TODO @gfichtenholt: I really wanted to put helm plugin's implementation of AvailablePackageDetailFromChart()
// here, and use it in flux plugin as well. But I found out a couple of flaws in the implementation and decided
// against it. Namely:
// 1. This assumption:
//    // We assume that chart.ChartVersions[0] will always contain either: the latest version or
//    // the specified version
//  This is a hack and forces the caller to prepate the input argument in a certain way such that
//  chart.ChartVersions[0] will be the desired version. I other words, the abstraction between the caller and
//  the call site is completely broken here. Yuck.
// 2. IMHO, it would have been better to get most of the detail info, including the current version out of
//   parsed chart YAML file, which this implementation chooses to ignore.
// I did consider using flux's implementation of AvailablePackageDetailFromChart but did not feel comfortable
// chaning helm plugin to use it before talking to @minelson
// Update Michael replied he is okay with my proposal:
// https://github.com/kubeapps/kubeapps/pull/4094#discussion_r790349962.
// Will come back to this

// GetUnescapedChartID takes a chart id with URI-encoded characters and decode them. Ex: 'foo%2Fbar' becomes 'foo/bar'
// also checks that the chart ID is in the expected format, namely "repoName/chartName"
func GetUnescapedChartID(chartID string) (string, error) {
	unescapedChartID, err := url.QueryUnescape(chartID)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "Unable to decode chart ID chart: %v", chartID)
	}
	// TODO(agamez): support ID with multiple slashes, eg: aaa/bbb/ccc
	chartIDParts := strings.Split(unescapedChartID, "/")
	if len(chartIDParts) != 2 {
		return "", status.Errorf(codes.InvalidArgument, "Incorrect package ref dentifier, currently just 'foo/bar' patterns are supported: %s", chartID)
	}
	return unescapedChartID, nil
}

func SplitChartIdentifier(chartID string) (repoName, chartName string, err error) {
	// getUnescapedChartID also ensures that there are two parts (ie. repo/chart-name only)
	unescapedChartID, err := GetUnescapedChartID(chartID)
	if err != nil {
		return "", "", err
	}
	chartIDParts := strings.Split(unescapedChartID, "/")
	return chartIDParts[0], chartIDParts[1], nil
}

// DefaultValuesFromSchema returns a yaml string with default values generated from an OpenAPI v3 Schema
func DefaultValuesFromSchema(schema []byte, isCommentedOut bool) (string, error) {
	if len(schema) == 0 {
		return "", nil
	}
	// Deserialize the schema passed into the function
	jsonSchemaProps := &apiextensions.JSONSchemaProps{}
	if err := yaml.Unmarshal(schema, jsonSchemaProps); err != nil {
		return "", err
	}
	structural, err := structuralschema.NewStructural(jsonSchemaProps)
	if err != nil {
		return "", err
	}

	// Generate the default values
	unstructuredDefaultValues := make(map[string]interface{})
	defaultValues(unstructuredDefaultValues, structural)
	yamlDefaultValues, err := yaml.Marshal(unstructuredDefaultValues)
	if err != nil {
		return "", err
	}
	strYamlDefaultValues := string(yamlDefaultValues)

	// If isCommentedOut, add a yaml comment character '#' to the beginning of each line
	if isCommentedOut {
		var sb strings.Builder
		scanner := bufio.NewScanner(strings.NewReader(strYamlDefaultValues))
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {
			sb.WriteString("# ")
			sb.WriteString(fmt.Sprintln(scanner.Text()))
		}
		strYamlDefaultValues = sb.String()
	}
	return strYamlDefaultValues, nil
}

// defaultValuesFromSchema returns a yaml string with default values generated from an OpenAPI v3 Schema
func defaultValuesFromSchema(schema []byte, isCommentedOut bool) (string, error) {
	if len(schema) == 0 {
		return "", nil
	}
	// Deserialize the schema passed into the function
	jsonSchemaProps := &apiextensions.JSONSchemaProps{}
	if err := yaml.Unmarshal(schema, jsonSchemaProps); err != nil {
		return "", err
	}
	structural, err := structuralschema.NewStructural(jsonSchemaProps)
	if err != nil {
		return "", err
	}

	// Generate the default values
	unstructuredDefaultValues := make(map[string]interface{})
	defaultValues(unstructuredDefaultValues, structural)
	yamlDefaultValues, err := yaml.Marshal(unstructuredDefaultValues)
	if err != nil {
		return "", err
	}
	strYamlDefaultValues := string(yamlDefaultValues)

	// If isCommentedOut, add a yaml comment character '#' to the beginning of each line
	if isCommentedOut {
		var sb strings.Builder
		scanner := bufio.NewScanner(strings.NewReader(strYamlDefaultValues))
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {
			sb.WriteString("# ")
			sb.WriteString(fmt.Sprintln(scanner.Text()))
		}
		strYamlDefaultValues = sb.String()
	}
	return strYamlDefaultValues, nil
}

// Default does defaulting of x depending on default values in s.
// Based upon https://github.com/kubernetes/apiextensions-apiserver/blob/release-1.21/pkg/apiserver/schema/defaulting/algorithm.go
// Plus modifications from https://github.com/vmware-tanzu/tanzu-framework/pull/1422
// In short, it differs from upstream in that:
// -- 1. Prevent deep copy of int as it panics
// -- 2. For type object scan the first level properties for any defaults to create an empty map to populate
// -- 3. If the property does not have a default, add one based on the type ("", false, etc)
func defaultValues(x interface{}, s *structuralschema.Structural) {
	if s == nil {
		return
	}

	switch x := x.(type) {
	case map[string]interface{}:
		for k, prop := range s.Properties { //nolint
			// if Default for object is nil, scan first level of properties for any defaults to create an empty default
			if prop.Default.Object == nil {
				createDefault := false
				if prop.Properties != nil {
					for _, v := range prop.Properties { //nolint
						if v.Default.Object != nil {
							createDefault = true
							break
						}
					}
				}
				if createDefault {
					prop.Default.Object = make(map[string]interface{})
					// If not generating an empty object, fall back to the data type's defaults
				} else {
					switch prop.Type {
					case "string":
						prop.Default.Object = ""
					case "number":
						prop.Default.Object = 0
					case "integer":
						prop.Default.Object = 0
					case "boolean":
						prop.Default.Object = false
					case "array":
						prop.Default.Object = []interface{}{}
					case "object":
						prop.Default.Object = make(map[string]interface{})
					}
				}
			}
			if _, found := x[k]; !found || isNonNullableNull(x[k], &prop) {
				if isKindInt(prop.Default.Object) {
					x[k] = prop.Default.Object
				} else {
					x[k] = runtime.DeepCopyJSONValue(prop.Default.Object)
				}
			}
		}
		for k := range x {
			if prop, found := s.Properties[k]; found {
				defaultValues(x[k], &prop)
			} else if s.AdditionalProperties != nil {
				if isNonNullableNull(x[k], s.AdditionalProperties.Structural) {
					if isKindInt(s.AdditionalProperties.Structural.Default.Object) {
						x[k] = s.AdditionalProperties.Structural.Default.Object
					} else {
						x[k] = runtime.DeepCopyJSONValue(s.AdditionalProperties.Structural.Default.Object)
					}
				}
				defaultValues(x[k], s.AdditionalProperties.Structural)
			}
		}
	case []interface{}:
		for i := range x {
			if isNonNullableNull(x[i], s.Items) {
				if isKindInt(s.Items.Default.Object) {
					x[i] = s.Items.Default.Object
				} else {
					x[i] = runtime.DeepCopyJSONValue(s.Items.Default.Object)
				}
			}
			defaultValues(x[i], s.Items)
		}
	default:
		// scalars, do nothing
	}
}

// isNonNullalbeNull returns true if the item is nil AND it's nullable
func isNonNullableNull(x interface{}, s *structuralschema.Structural) bool {
	return x == nil && s != nil && !s.Generic.Nullable
}

// isKindInt returns true if the item is an int
func isKindInt(src interface{}) bool {
	return src != nil && reflect.TypeOf(src).Kind() == reflect.Int
}
