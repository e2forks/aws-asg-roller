package main

import (
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
)

func setAsgDesired(svc autoscalingiface.AutoScalingAPI, asg *autoscaling.Group, count int64) error {
	// increase the desired capacity by 1
	desiredInput := &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: asg.AutoScalingGroupName,
		DesiredCapacity:      aws.Int64(count),
		HonorCooldown:        aws.Bool(true),
	}

	_, err := svc.SetDesiredCapacity(desiredInput)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case autoscaling.ErrCodeScalingActivityInProgressFault:
				return fmt.Errorf("%s %v", autoscaling.ErrCodeScalingActivityInProgressFault, aerr.Error())
			case autoscaling.ErrCodeResourceContentionFault:
				return fmt.Errorf("%s %v", autoscaling.ErrCodeResourceContentionFault, aerr.Error())
			default:
				return fmt.Errorf("Unexpected and unknown AWS error: %v", aerr.Error())
			}
		} else {
			return fmt.Errorf("Unexpected and unknown non-AWS error: %v", err.Error())
		}
	}
	return nil
}

func awsGetHostname(svc ec2iface.EC2API, id string) (string, error) {
	hostnames, err := awsGetHostnames(svc, []string{id})
	if err != nil {
		return "", err
	}
	if len(hostnames) < 1 {
		return "", err
	}
	return hostnames[0], nil
}
func awsGetHostnames(svc ec2iface.EC2API, ids []string) ([]string, error) {
	ec2input := &ec2.DescribeInstancesInput{
		InstanceIds: aws.StringSlice(ids),
	}
	nodesResult, err := svc.DescribeInstances(ec2input)
	if err != nil {
		return nil, fmt.Errorf("Unable to get description for node %v: %v", ids, err)
	}
	if len(nodesResult.Reservations) < 1 {
		return nil, fmt.Errorf("Did not get any reservations for node %v", ids)
	}
	hostnames := make([]string, 0)
	for _, i := range nodesResult.Reservations {
		for _, j := range i.Instances {
			hostnames = append(hostnames, *j.PrivateDnsName)
		}
	}
	return hostnames, nil
}
func awsEc2DeleteTags(svc ec2iface.EC2API, ids []string, key string) error {
	ec2DeleteTags := &ec2.DeleteTagsInput{
		Resources: aws.StringSlice(ids),
		Tags: []*ec2.Tag{
			{
				Key: aws.String(key),
			},
		},
	}
	_, err := svc.DeleteTags(ec2DeleteTags)
	if err != nil {
		return fmt.Errorf("Unable to get Delete Tag for node %v: %v", ids, err)
	}
	return nil
}
func awsAsgCreateOrUpdateTags(svc autoscalingiface.AutoScalingAPI, asg *autoscaling.Group, key string, value string) error {
	input := &autoscaling.CreateOrUpdateTagsInput{
		Tags: []*autoscaling.Tag{
			{
				Key:               aws.String(key),
				PropagateAtLaunch: aws.Bool(true),
				ResourceId:        asg.AutoScalingGroupName,
				ResourceType:      aws.String("auto-scaling-group"),
				Value:             aws.String(value),
			},
		},
	}
	_, err := svc.CreateOrUpdateTags(input)
	if err != nil {
		return fmt.Errorf("Unable to get Create/Update Tag for asg %v: %v", asg.AutoScalingGroupName, err)
	}
	return nil
}
func awsAsgDeleteTags(svc autoscalingiface.AutoScalingAPI, asg *autoscaling.Group, key string) error {
	asgDeleteTags := &autoscaling.DeleteTagsInput{
		Tags: []*autoscaling.Tag{
			{
				Key:          aws.String(key),
				ResourceId:   asg.AutoScalingGroupName,
				ResourceType: aws.String("auto-scaling-group"),
			},
		},
	}
	_, err := svc.DeleteTags(asgDeleteTags)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case autoscaling.ErrCodeResourceInUseFault:
				// don't block but cannot throw error anymore
				go func(svc autoscalingiface.AutoScalingAPI, asg *autoscaling.Group, key string) {
					time.Sleep(300 * time.Millisecond)
					awsAsgDeleteTags(svc, asg, key)
				}(svc, asg, key)
			default:
				return fmt.Errorf("Unable to get Create/Update Tag for asg %v: %v", asg.AutoScalingGroupName, err)
			}
		}
	}
	return nil
}

func awsDescribeGroups(svc autoscalingiface.AutoScalingAPI, names []string) ([]*autoscaling.Group, error) {
	input := &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: aws.StringSlice(names),
	}
	result, err := svc.DescribeAutoScalingGroups(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case autoscaling.ErrCodeInvalidNextToken:
				return nil, fmt.Errorf("Unexpected AWS NextToken error when doing non-pagination describe")
			case autoscaling.ErrCodeResourceContentionFault:
				return nil, fmt.Errorf("Unexpected AWS ResourceContentionFault when doing describe")
			default:
				return nil, fmt.Errorf("Unexpected and unknown AWS error when doing describe: %v", aerr)
			}
		} else {
			// Print the error, cast err to awserr.Error to get the Code and
			// Message from an error.
			return nil, fmt.Errorf("Unexpected and unknown non-AWS error when doing describe: %v", err.Error())
		}
	}
	return result.AutoScalingGroups, nil
}

func awsTerminateNode(svc autoscalingiface.AutoScalingAPI, id string) error {
	input := &autoscaling.TerminateInstanceInAutoScalingGroupInput{
		InstanceId:                     aws.String(id),
		ShouldDecrementDesiredCapacity: aws.Bool(false),
	}

	_, err := svc.TerminateInstanceInAutoScalingGroup(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case autoscaling.ErrCodeScalingActivityInProgressFault:
				return fmt.Errorf("Could not terminate instance, autoscaling already in progress, will try next loop")
			case autoscaling.ErrCodeResourceContentionFault:
				return fmt.Errorf("Could not terminate instance, instance in contention, will try next loop")
			default:
				return fmt.Errorf("Unknown aws error when terminating old instance: %v", aerr.Error())
			}
		} else {
			// Print the error, cast err to awserr.Error to get the Code and
			// Message from an error.
			return fmt.Errorf("Unknown non-aws error when terminating old instance: %v", err.Error())
		}
	}
	return nil
}

func awsGetServices() (ec2iface.EC2API, autoscalingiface.AutoScalingAPI, error) {
	sess, err := session.NewSession()
	if err != nil {
		return nil, nil, err
	}
	asgSvc := autoscaling.New(sess)
	ec2svc := ec2.New(sess)
	return ec2svc, asgSvc, nil
}
