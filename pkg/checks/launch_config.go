package checks

import (
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
)

// DesiredLaunchConfiguration describes the desired and current state of the asg.
type DesiredLaunchConfiguration struct {
	ec2Svc                  ec2iface.EC2API
	LaunchConfigurationName *string
}

// NewDesiredLaunchConfiguration create a new instance of NewDesiredLaunchConfiguration.
func NewDesiredLaunchConfiguration(ec2Svc ec2iface.EC2API, desired *autoscaling.Group) DesiredLaunchConfiguration {
	return DesiredLaunchConfiguration{ec2Svc: ec2Svc, LaunchConfigurationName: desired.LaunchConfigurationName}
}

// InstanceNeedsUpdate checks whether an autoscaling instance needs to be flagged for update.
func (p *DesiredLaunchConfiguration) InstanceNeedsUpdate(instance *autoscaling.Instance) bool {
	return p.LaunchConfigurationName == nil || *p.LaunchConfigurationName != *instance.LaunchConfigurationName
}
