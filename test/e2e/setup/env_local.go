package setup

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"
)

const envFile = "../../.env.local"

type envMap map[string]string

func parseEnvLocal() envMap {
	env := make(envMap)
	b, err := os.ReadFile(envFile)
	if err != nil {
		log.Fatal(err)
	}
	str := string(b)
	lines := strings.Split(str, "\n")
	for idx, line := range lines {
		if line == "" {
			continue
		}
		lineParts := strings.Split(line, "=")
		if len(lineParts) != 2 {
			log.Fatalf("invalid line %d", idx)
		}
		envName, envValue := lineParts[0], lineParts[1]
		env[strings.ToUpper(envName)] = envValue[1 : len(envValue)-1]
	}
	return env
}

var envs *envMap = nil

func GetEnv(key string) string {
	if envs == nil {
		envMap := parseEnvLocal()
		envs = &envMap
	}
	if val, ok := (*envs)[key]; ok {
		return val
	}
	return os.Getenv(key)
}

func SetEnv(newEnvs envMap) {
	if envs == nil {
		envMap := parseEnvLocal()
		envs = &envMap
	}
	for key, value := range newEnvs {
		(*envs)[key] = value
	}

	f, err := os.OpenFile(envFile, os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	writer := bufio.NewWriter(f)

	for key, value := range newEnvs {
		_, err := writer.WriteString(fmt.Sprintf("%s='%s'\n", strings.ToUpper(key), value))
		if err != nil {
			log.Fatal(err)
		}
	}
	err = writer.Flush()
	if err != nil {
		log.Fatal(err)
	}
}
