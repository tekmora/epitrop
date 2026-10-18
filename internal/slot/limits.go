package slot

import (
	"context"
	"errors"
	"fmt"

	"github.com/tekmora/epitrop/internal/config"
)

// Limits are the memory limits a slot's template sets for its job, in systemd's size form,
// and the environment names the job takes from its runner.
type Limits struct {
	// Max is the job's sandbox.memory_max: the hard limit.
	Max string
	// High is the job's sandbox.memory_high: the soft limit.
	High string
	// EnvAllow is the job's sandbox.env_allow: the names a detached runner's service must
	// carry from the caller, or the job never sees their values.
	EnvAllow []string
}

// TemplateLimits reads the memory limits that a slot's template writes out for its job. A
// detached run takes them for the runner's own service as well, so both must stand in the
// template itself: the default the loader fills in for a missing memory_max is no limit
// that anyone chose for the slot, and a missing memory_high leaves the soft limit unset.
func TemplateLimits(ctx context.Context, template string) (Limits, error) {
	doc, err := config.ReadDocument(ctx, template)
	if err != nil {
		return Limits{}, err
	}
	limits, err := templateLimits(doc)
	if err != nil {
		return Limits{}, fmt.Errorf("slot: template %s: %w", template, err)
	}
	if err = config.CheckMemoryLimits(limits.Max, limits.High); err != nil {
		return Limits{}, fmt.Errorf("slot: template %s: sandbox.%w", template, err)
	}
	return limits, nil
}

func templateLimits(doc map[string]any) (Limits, error) {
	jobs, isList := doc["jobs"].([]any)
	if !isList || len(jobs) != 1 {
		return Limits{}, errors.New("a slot's template holds exactly one job")
	}
	job, isJob := jobs[0].(map[string]any)
	if !isJob {
		return Limits{}, errors.New("its job is no object")
	}
	sandbox, isSandbox := job["sandbox"].(map[string]any)
	if !isSandbox {
		return Limits{}, errors.New("its job sets no sandbox.memory_max and sandbox.memory_high, which a detached run requires")
	}
	var limits Limits
	values := []struct {
		key  string
		into *string
	}{{"memory_max", &limits.Max}, {"memory_high", &limits.High}}
	for i := 0; i < len(values); i++ {
		value, isString := sandbox[values[i].key].(string)
		if !isString || value == "" {
			return Limits{}, fmt.Errorf("its job sets no sandbox.%s, which a detached run requires", values[i].key)
		}
		*values[i].into = value
	}
	allow, err := templateEnvAllow(sandbox["env_allow"])
	if err != nil {
		return Limits{}, err
	}
	limits.EnvAllow = allow
	return limits, nil
}

// templateEnvAllow reads sandbox.env_allow as written; the loader checks the names when the
// runner loads the template.
func templateEnvAllow(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	list, isList := value.([]any)
	if !isList || len(list) > config.MaxSandboxEnvAllow {
		return nil, fmt.Errorf("its job's sandbox.env_allow is no list of at most %d names", config.MaxSandboxEnvAllow)
	}
	names := make([]string, 0, len(list))
	for i := 0; i < len(list); i++ {
		name, isString := list[i].(string)
		if !isString {
			return nil, fmt.Errorf("its job's sandbox.env_allow/%d is no name", i)
		}
		names = append(names, name)
	}
	return names, nil
}
