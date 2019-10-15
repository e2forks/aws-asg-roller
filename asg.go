package main

import (
	"fmt"

	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
)

// AsgGroup describes an AWS autoscaling group than needs to be managed by
// aws-asg-roller.
type AsgGroup struct {
	asg    *autoscaling.Group
	ec2Svc ec2iface.EC2API
}

// Name returns the AutoScalingGroupName of the ASG.
func (a *AsgGroup) Name() *string {
	return a.asg.AutoScalingGroupName
}

// GetLaunchTemplateFromSpec retrieves the asg launch template from a LaunchTemplateSpecification.
func (a *AsgGroup) GetLaunchTemplateFromSpec(launchTemplateSpec *autoscaling.LaunchTemplateSpecification) (*ec2.LaunchTemplate, error) {
	var (
		targetTemplate *ec2.LaunchTemplate
		err            error
	)
	switch {
	case launchTemplateSpec.LaunchTemplateId != nil && *launchTemplateSpec.LaunchTemplateId != "":
		if targetTemplate, err = awsGetLaunchTemplateByID(a.ec2Svc, *launchTemplateSpec.LaunchTemplateId); err != nil {
			return nil, fmt.Errorf("error retrieving information about launch template ID %s: %v", *launchTemplateSpec.LaunchTemplateId, err)
		}
	case launchTemplateSpec.LaunchTemplateName != nil && *launchTemplateSpec.LaunchTemplateName != "":
		if targetTemplate, err = awsGetLaunchTemplateByName(a.ec2Svc, *launchTemplateSpec.LaunchTemplateName); err != nil {
			return nil, fmt.Errorf("error retrieving information about launch template name %s: %v", *launchTemplateSpec.LaunchTemplateName, err)
		}
	default:
		return nil, fmt.Errorf("AutoScaling Group %s had invalid Launch Template", a.Name())
	}
	// extra safety check
	if targetTemplate == nil {
		return nil, fmt.Errorf("no template found")
	}

	return targetTemplate, nil
}
