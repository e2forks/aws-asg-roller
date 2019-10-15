package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	asgCheckDelay = 30 // Default delay between checks of ASG status in seconds
)

func main() {

	// handles arg flags
	debug := flag.Bool("debug", false, "sets log level to debug")
	human := flag.Bool("human", false, "sets log output to human readable format")
	flag.Parse()

	// default logging level is info
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	if *debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
	if *human {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	asgList := strings.Split(os.Getenv("ROLLER_ASG"), ",")
	if asgList == nil || len(asgList) == 0 {
		log.Fatal().Msgf("Must supply at least one ASG in ROLLER_ASG environment variable")
	}

	// get config env
	ignoreDaemonSets := os.Getenv("ROLLER_IGNORE_DAEMONSETS") != "false"
	deleteLocalData := os.Getenv("ROLLER_DELETE_LOCAL_DATA") != "false"
	// get a kube connection
	readinessHandler, err := kubeGetReadinessHandler(ignoreDaemonSets, deleteLocalData)
	if err != nil {
		log.Fatal().Err(err).Msgf("Error getting kubernetes readiness handler when required: %v", err)
	}

	// get the AWS sessions
	ec2Svc, asgSvc, err := awsGetServices()
	if err != nil {
		log.Fatal().Err(err).Msgf("Unable to create an AWS session: %v", err)
	}

	// to keep track of original target sizes during rolling updates
	originalDesired := map[string]int64{}

	checkDelay, err := getDelay()
	if err != nil {
		log.Fatal().Err(err).Msgf("Unable to get delay: %s", err.Error())
	}

	// infinite loop
	for {
		err := adjust(asgList, ec2Svc, asgSvc, readinessHandler, originalDesired)
		if err != nil {
			log.Warn().Msgf("Error adjusting AutoScaling Groups: %v", err)
		}
		// delay with each loop
		log.Info().Msgf("Sleeping %d seconds\n", checkDelay)
		time.Sleep(time.Duration(checkDelay) * time.Second)
	}
}

// Returns delay value to use in loop. Uses default if not defined.
func getDelay() (int, error) {
	delayOverride, exist := os.LookupEnv("ROLLER_CHECK_DELAY")
	if exist {
		delay, err := strconv.Atoi(delayOverride)
		if err != nil {
			return -1, fmt.Errorf("ROLLER_CHECK_DELAY is not parsable: %v (%s)", delayOverride, err.Error())
		}
		return delay, nil
	}

	return asgCheckDelay, nil
}
