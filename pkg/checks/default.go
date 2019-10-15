package checks

import (
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
)

// GetDefaultChecker returns the default checker to determine if an instance needs
// to be updated. The default checker is to check the LaunchTemplate, and only
// the LaunchConfiguration if the asg does not have a LaunchTemplate.
func GetDefaultChecker(ec2Svc ec2iface.EC2API, asg *autoscaling.Group) func(*autoscaling.Instance) bool {
	desiredLT, err := NewDesiredLaunchTemplate(ec2Svc, asg)
	if err == nil {
		return desiredLT.InstanceNeedsUpdate
	}
	desiredLC := NewDesiredLaunchConfiguration(ec2Svc, asg)
	return desiredLC.InstanceNeedsUpdate
}
