package main

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
)

const (
	healthy = "Healthy"
)

// adjust runs a single adjustment in the loop to update an ASG in a rolling fashion to latest launch config
func adjust(asgList []string, ec2Svc ec2iface.EC2API, asgSvc autoscalingiface.AutoScalingAPI, readinessHandler readiness, originalDesired map[string]int64) (string, error) {
	// return msg
	msg := ""
	// get information on all of the groups
	asgs, err := awsDescribeGroups(asgSvc, asgList)
	if err != nil {
		return "", fmt.Errorf("Unexpected error describing ASGs, skipping: %v", err)
	}
	asgMap := map[string]*autoscaling.Group{}
	// get information on all of the ec2 instances
	instances := make([]*autoscaling.Instance, 0)
	for _, asg := range asgs {
		oldI, newI, err := groupInstances(asg, ec2Svc)
		if err != nil {
			return "", fmt.Errorf("unable to group instances into new and old: %v", err)
		}
		// if there are no outdated instances skip updating
		if len(oldI) == 0 {
			// remove tag: cluster-autoscaler.kubernetes.io/scale-down-disabled
			// so autoscaler can function normally again
			awsAsgDeleteTags(asgSvc, asg, "cluster-autoscaler.kubernetes.io/scale-down-disabled")
			err := awsEc2DeleteTags(ec2Svc, mapInstancesIds(asg.Instances), "cluster-autoscaler.kubernetes.io/scale-down-disabled")
			if err != nil {
				msg += fmt.Sprintln("Skipped: %v", err)
			}
			continue
		}

		asgMap[*asg.AutoScalingGroupName] = asg
		instances = append(instances, oldI...)
		instances = append(instances, newI...)

		// set Tag: cluster-autoscaler.kubernetes.io/scale-down-disabled to true
		// so that autoscaler will not scale down any new nodes
		err = awsAsgCreateOrUpdateTags(asgSvc, asg, "cluster-autoscaler.kubernetes.io/scale-down-disabled", "true")
		if err != nil {
			msg += fmt.Sprintln("Skipped: %v", err)
		}
	}
	// no instances no work needed
	if len(instances) == 0 {
		return "All asgs are up-to-date. No work required.", nil
	}
	ids := mapInstancesIds(instances)
	hostnames, err := awsGetHostnames(ec2Svc, ids)
	if err != nil {
		return "", fmt.Errorf("Unable to get aws hostnames for ids %v: %v", ids, err)
	}
	hostnameMap := map[string]string{}
	for i, id := range ids {
		hostnameMap[id] = hostnames[i]
	}
	newDesired := map[string]int64{}
	newTerminate := map[string]string{}
	newOriginalDesired := map[string]int64{}
	errors := map[*string]error{}

	// keep keyed references to the ASGs
	for _, asg := range asgMap {
		newDesiredA, newOriginalA, terminateID, err := calculateAdjustment(asg, ec2Svc, hostnameMap, readinessHandler, originalDesired[*asg.AutoScalingGroupName])
		newDesired[*asg.AutoScalingGroupName] = newDesiredA
		newOriginalDesired[*asg.AutoScalingGroupName] = newOriginalA
		if terminateID != "" {
			newTerminate[*asg.AutoScalingGroupName] = terminateID
		}
		errors[asg.AutoScalingGroupName] = err
	}
	// adjust original desired
	for asg, desired := range newOriginalDesired {
		originalDesired[asg] = desired
	}
	// adjust current desired
	for asg, desired := range newDesired {
		err = setAsgDesired(asgSvc, asgMap[asg], desired)
		if err != nil {
			return "", fmt.Errorf("Error setting desired to %d for ASG %s: %v", desired, asg, err)
		}
	}
	// terminate nodes
	for asg, id := range newTerminate {
		// all new config instances are ready, terminate an old one
		err = awsTerminateNode(asgSvc, id)
		if err != nil {
			return "", fmt.Errorf("Error terminating node %s in ASG %s: %v", id, asg, err)
		}
	}
	return msg, nil
}

// calculateAdjustment calculates the new settings for the desired number, and which node (if any) to terminate
// this makes no actual adjustment, only calculates what new settings should be
// returns:
//   what the new desired number of instances should be
//   what the new original desired should be, primarily if it should be reset
//   ID of an instance to terminate, "" if none
//   error
func calculateAdjustment(asg *autoscaling.Group, ec2Svc ec2iface.EC2API, hostnameMap map[string]string, readinessHandler readiness, originalDesired int64) (int64, int64, string, error) {
	desired := *asg.DesiredCapacity

	// get instances with old launch config
	oldInstances, newInstances, err := groupInstances(asg, ec2Svc)
	if err != nil {
		return originalDesired, 0, "", fmt.Errorf("unable to group instances into new and old: %v", err)
	}

	// Possibilities:
	// 1- we have some old ones, but have not started updates yet: set the desired, increment and loop
	// 2- we have no old ones, but have started updates: we must be at end, so finish
	// 3- we have some old ones, but have started updates: run the updates
	if len(oldInstances) == 0 {
		if originalDesired > 0 {
			return originalDesired, 0, "", nil
		}
	}
	if originalDesired == 0 {
		return desired + 1, desired, "", nil
	}

	// how we determine if we can terminate one
	// we already know we have increased desired capacity
	// check if:
	// a- actual instance count matches our new desired
	// b- all new config instances are in valid state
	// if yes, terminate one old one
	// if not, loop around again - eventually it will be

	// do we have at least one more more ready instances than the original desired? if not, loop again until we do
	readyCount := 0
	for _, i := range asg.Instances {
		if *i.HealthStatus == healthy {
			readyCount++
		}

	}
	if int64(readyCount) < originalDesired+1 {
		return desired, originalDesired, "", nil
	}
	// are any of the updated config instances not ready?
	unReadyCount := 0
	// should check if new node *really* is ready to function
	for _, i := range newInstances {
		if *i.HealthStatus != healthy {
			unReadyCount++
		}
	}
	if unReadyCount > 0 {
		return desired, originalDesired, "", nil
	}
	// do we have additional requirements for readiness?
	if readinessHandler != nil {
		var (
			hostnames []string
			err       error
		)
		// check if the new nodes all are in ready state
		ids := mapInstancesIds(newInstances)
		hostnames = make([]string, 0)
		for _, i := range ids {
			hostnames = append(hostnames, hostnameMap[i])
		}
		unReadyCount, err = readinessHandler.getUnreadyCount(hostnames, ids)
		if err != nil {
			return desired, originalDesired, "", fmt.Errorf("Error getting readiness new node status: %v", err)
		}
		if unReadyCount > 0 {
			return desired, originalDesired, "", nil
		}
	}
	candidate := *oldInstances[0].InstanceId

	if readinessHandler != nil {
		// get the node reference - first need the hostname
		var (
			hostname string
			err      error
		)
		hostname = hostnameMap[candidate]
		err = readinessHandler.prepareTermination([]string{hostname}, []string{candidate})
		if err != nil {
			return desired, originalDesired, "", fmt.Errorf("Unexpected error readiness handler terminating node %s: %v", hostname, err)
		}
	}

	// all new config instances are ready, terminate an old one
	return desired, originalDesired, candidate, nil
}

// groupInstances handles all of the logic for determining which nodes in the ASG have an old or outdated
// config, and which are up to date. It should to nothing else.
// The entire rest of the code should rely on this for making the determination
func groupInstances(asg *autoscaling.Group, ec2Svc ec2iface.EC2API) ([]*autoscaling.Instance, []*autoscaling.Instance, error) {
	oldInstances := make([]*autoscaling.Instance, 0)
	newInstances := make([]*autoscaling.Instance, 0)
	// we want to be able to handle LaunchTemplate as well
	targetLc := asg.LaunchConfigurationName
	targetLt := asg.LaunchTemplate
	// prioritize LaunchTemplate over LaunchConfiguration
	if targetLt != nil {
		// we are using LaunchTemplate. Unlike LaunchConfiguration, you can have two nodes in the ASG
		//  with the same LT name, same ID but different versions, so need to check version.
		//  they even can have the same version, if the version is `$Latest` or `$Default`, so need
		//  to get actual versions for each
		var (
			targetTemplate *ec2.LaunchTemplate
			err            error
		)
		switch {
		case targetLt.LaunchTemplateId != nil && *targetLt.LaunchTemplateId != "":
			if targetTemplate, err = awsGetLaunchTemplateByID(ec2Svc, *targetLt.LaunchTemplateId); err != nil {
				return nil, nil, fmt.Errorf("error retrieving information about launch template ID %s: %v", *targetLt.LaunchTemplateId, err)
			}
		case targetLt.LaunchTemplateName != nil && *targetLt.LaunchTemplateName != "":
			if targetTemplate, err = awsGetLaunchTemplateByName(ec2Svc, *targetLt.LaunchTemplateName); err != nil {
				return nil, nil, fmt.Errorf("error retrieving information about launch template name %s: %v", *targetLt.LaunchTemplateName, err)
			}
		default:
			return nil, nil, fmt.Errorf("AutoScaling Group %s had invalid Launch Template", *asg.AutoScalingGroupName)
		}
		// extra safety check
		if targetTemplate == nil {
			return nil, nil, fmt.Errorf("no template found")
		}
		// now we can loop through each node and compare
		for _, i := range asg.Instances {
			switch {
			case i.LaunchTemplate == nil:
				// has no launch template at all
				oldInstances = append(oldInstances, i)
			case aws.StringValue(i.LaunchTemplate.LaunchTemplateName) != aws.StringValue(targetLt.LaunchTemplateName):
				// mismatched named
				oldInstances = append(oldInstances, i)
			case aws.StringValue(i.LaunchTemplate.LaunchTemplateId) != aws.StringValue(targetLt.LaunchTemplateId):
				// mismatched ID
				oldInstances = append(oldInstances, i)
			// name and id match, just need to check versions
			case !compareLaunchTemplateVersions(targetTemplate, targetLt, i.LaunchTemplate):
				oldInstances = append(oldInstances, i)
			default:
				newInstances = append(newInstances, i)
			}
		}
	} else {
		// go through each instance and find those that are not with the target LC
		for _, i := range asg.Instances {
			if i.LaunchConfigurationName != nil && *i.LaunchConfigurationName == *targetLc {
				newInstances = append(newInstances, i)
			} else {
				oldInstances = append(oldInstances, i)
			}
		}
	}
	return oldInstances, newInstances, nil
}

func mapInstancesIds(instances []*autoscaling.Instance) []string {
	ids := make([]string, 0)
	for _, i := range instances {
		ids = append(ids, *i.InstanceId)
	}
	return ids
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
