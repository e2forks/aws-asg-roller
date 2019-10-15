package checks

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"

	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
)

// DesiredLaunchTemplate describes the desired and current state of the asg.
type DesiredLaunchTemplate struct {
	ec2Svc                      ec2iface.EC2API
	LaunchTemplate              *ec2.LaunchTemplate
	LaunchTemplateSpecification *autoscaling.LaunchTemplateSpecification
}

// NewDesiredLaunchTemplate create a new instance of DesiredLaunchTemplate
func NewDesiredLaunchTemplate(ec2Svc ec2iface.EC2API, desired *autoscaling.Group) (DesiredLaunchTemplate, error) {
	template, err := getLaunchTemplateFromAsg(ec2Svc, desired)
	if err != nil {
		return DesiredLaunchTemplate{}, err
	}

	return DesiredLaunchTemplate{ec2Svc: ec2Svc, LaunchTemplate: template, LaunchTemplateSpecification: desired.LaunchTemplate}, nil
}

// InstanceNeedsUpdate checks whether an autoscaling instance needs to be flagged for update.
func (p *DesiredLaunchTemplate) InstanceNeedsUpdate(instance *autoscaling.Instance) bool {

	template := instance.LaunchTemplate
	return (instance.LaunchTemplate == nil ||
		// check template name
		aws.StringValue(instance.LaunchTemplate.LaunchTemplateName) != aws.StringValue(template.LaunchTemplateName) ||
		// check template id
		aws.StringValue(instance.LaunchTemplate.LaunchTemplateId) != aws.StringValue(template.LaunchTemplateId) ||
		// check template version
		!compareLaunchTemplateVersions(p.LaunchTemplate, p.LaunchTemplateSpecification, instance.LaunchTemplate))
}

// getLaunchTemplateFromAsg retrieves the asg launch template from an autoscaling group.
func getLaunchTemplateFromAsg(ec2Svc ec2iface.EC2API, asg *autoscaling.Group) (*ec2.LaunchTemplate, error) {
	launchTemplateSpec := asg.LaunchTemplate
	var (
		targetTemplate *ec2.LaunchTemplate
		err            error
	)
	switch {
	case launchTemplateSpec.LaunchTemplateId != nil && *launchTemplateSpec.LaunchTemplateId != "":
		if targetTemplate, err = awsGetLaunchTemplateByID(ec2Svc, *launchTemplateSpec.LaunchTemplateId); err != nil {
			return nil, fmt.Errorf("error retrieving information about launch template ID %s: %v", *launchTemplateSpec.LaunchTemplateId, err)
		}
	case launchTemplateSpec.LaunchTemplateName != nil && *launchTemplateSpec.LaunchTemplateName != "":
		if targetTemplate, err = awsGetLaunchTemplateByName(ec2Svc, *launchTemplateSpec.LaunchTemplateName); err != nil {
			return nil, fmt.Errorf("error retrieving information about launch template name %s: %v", *launchTemplateSpec.LaunchTemplateName, err)
		}
	default:
		return nil, fmt.Errorf("AutoScaling Group %s had invalid Launch Template", *asg.AutoScalingGroupName)
	}
	// extra safety check
	if targetTemplate == nil {
		return nil, fmt.Errorf("no template found")
	}

	return targetTemplate, nil
}

// compareLaunchTemplateVersions compare two launch template versions and see if they match
// can handle `$Latest` and `$Default` by resolving to the actual version in use
func compareLaunchTemplateVersions(targetTemplate *ec2.LaunchTemplate, lt1, lt2 *autoscaling.LaunchTemplateSpecification) bool {
	// if both versions do not start with `$`, then just compare
	if lt1 == nil && lt2 == nil {
		return true
	}
	if (lt1 == nil && lt2 != nil) || (lt1 != nil && lt2 == nil) {
		return false
	}
	if lt1.Version == nil && lt2.Version == nil {
		return true
	}
	if (lt1.Version == nil && lt2.Version != nil) || (lt1.Version != nil && lt2.Version == nil) {
		return false
	}
	// if either version starts with `$`, then resolve to actual version from LaunchTemplate
	var lt1version, lt2version string
	switch *lt1.Version {
	case "$Default":
		lt1version = fmt.Sprintf("%d", targetTemplate.DefaultVersionNumber)
	case "$Latest":
		lt1version = fmt.Sprintf("%d", targetTemplate.LatestVersionNumber)
	default:
		lt1version = *lt1.Version
	}
	switch *lt2.Version {
	case "$Default":
		lt2version = fmt.Sprintf("%d", targetTemplate.DefaultVersionNumber)
	case "$Latest":
		lt2version = fmt.Sprintf("%d", targetTemplate.LatestVersionNumber)
	default:
		lt2version = *lt2.Version
	}
	if lt1version != lt2version {
		return false
	}
	return true
}

func awsGetLaunchTemplateByName(svc ec2iface.EC2API, name string) (*ec2.LaunchTemplate, error) {
	input := &ec2.DescribeLaunchTemplatesInput{
		LaunchTemplateNames: []*string{
			aws.String(name),
		},
	}
	return awsGetLaunchTemplate(svc, input)
}

func awsGetLaunchTemplateByID(svc ec2iface.EC2API, id string) (*ec2.LaunchTemplate, error) {
	input := &ec2.DescribeLaunchTemplatesInput{
		LaunchTemplateIds: []*string{
			aws.String(id),
		},
	}
	return awsGetLaunchTemplate(svc, input)
}

func awsGetLaunchTemplate(svc ec2iface.EC2API, input *ec2.DescribeLaunchTemplatesInput) (*ec2.LaunchTemplate, error) {
	templatesOutput, err := svc.DescribeLaunchTemplates(input)
	descriptiveMsg := fmt.Sprintf("%v / %v", input.LaunchTemplateIds, input.LaunchTemplateNames)
	if err != nil {
		return nil, fmt.Errorf("Unable to get description for Launch Template %s: %v", descriptiveMsg, err)
	}
	if len(templatesOutput.LaunchTemplates) < 1 {
		return nil, nil
	}
	return templatesOutput.LaunchTemplates[0], nil
}
