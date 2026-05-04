package config

import (
	"fmt"
	"testing"
)

func TestConfig(t *testing.T) {
	c := InitConfig("../config.yaml")
	fmt.Println(c)
}
