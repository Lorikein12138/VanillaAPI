package service

import (
	"fmt"
	appTimezone "github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	name, testCase := os.Getenv("SUB2API_TEST_SERVICE_TIMEZONE"), os.Getenv("SUB2API_TEST_SERVICE_TIMEZONE_CASE")
	if (name == "") != (testCase == "") {
		fmt.Fprintln(os.Stderr, "invalid service timezone child markers")
		os.Exit(2)
	}
	if name == "" {
		name = "UTC"
	}
	if err := appTimezone.Init(name); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}
