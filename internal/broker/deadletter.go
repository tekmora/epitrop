package broker

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

const (
	// MaxClasses is the largest number of dead-letter classes one set may hold.
	MaxClasses = 32
	// MaxSignatures is the largest number of signatures one class may declare.
	MaxSignatures = 16
	// MaxSignatureLen is the longest signature a class may declare, in bytes.
	MaxSignatureLen = 512
	// MaxRetries is the most retries a class may grant a failure: one rerun at most, so a
	// rerun never hides a real regression for long.
	MaxRetries = 1
)

var (
	// ErrClassSet says a dead-letter class set is not valid.
	ErrClassSet = errors.New("broker: invalid dead-letter class set")
	// ErrUnknownClass says no class of the set has that name.
	ErrUnknownClass = errors.New("broker: unknown dead-letter class")

	classNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

// ClassSpec declares a dead-letter class. A failure belongs to the first class, in set order,
// one of whose signatures matches its text; the signatures are RE2 expressions in multi-line
// mode, so ^ and $ match at line ends. The fallback class has no signatures and takes every
// failure no other class matched.
type ClassSpec struct {
	Name       string
	Signatures []string
	MaxRetries int
}

// Failure is one failed attempt of a unit. Attempt counts from 1.
type Failure struct {
	Unit    string
	Text    string
	Attempt int
}

// Verdict says where a failure went: its class, and whether it may be retried. A failure
// that is not retried waits in its class's dead-letter queue.
type Verdict struct {
	Class string
	Retry bool
}

type deadLetterClass struct {
	spec       ClassSpec
	signatures []*regexp.Regexp
	queue      chan Failure
}

// DeadLetters classifies failures and parks the ones that get no retry, one bounded queue
// per class (HISS-06).
type DeadLetters struct {
	classes  []*deadLetterClass
	fallback *deadLetterClass
	byName   map[string]*deadLetterClass
}

// NewDeadLetters builds a class set. Exactly one class, fallback, has no signatures; every
// other class has at least one. Each class queue holds capacity failures.
func NewDeadLetters(specs []ClassSpec, fallback string, capacity int) (*DeadLetters, error) {
	if capacity < 1 || capacity > MaxCapacity {
		return nil, fmt.Errorf("%w: %d, want 1 to %d", ErrCapacity, capacity, MaxCapacity)
	}
	if len(specs) == 0 || len(specs) > MaxClasses {
		return nil, fmt.Errorf("%w: %d classes, want 1 to %d", ErrClassSet, len(specs), MaxClasses)
	}
	d := &DeadLetters{byName: make(map[string]*deadLetterClass, len(specs))}
	for _, spec := range specs {
		class, err := newDeadLetterClass(spec, spec.Name == fallback, capacity)
		if err != nil {
			return nil, err
		}
		if d.byName[spec.Name] != nil {
			return nil, fmt.Errorf("%w: class %s declared twice", ErrClassSet, spec.Name)
		}
		d.byName[spec.Name] = class
		if spec.Name == fallback {
			d.fallback = class
		} else {
			d.classes = append(d.classes, class)
		}
	}
	if d.fallback == nil {
		return nil, fmt.Errorf("%w: fallback class %s not declared", ErrClassSet, fallback)
	}
	return d, nil
}

func newDeadLetterClass(spec ClassSpec, fallback bool, capacity int) (*deadLetterClass, error) {
	if !classNamePattern.MatchString(spec.Name) {
		return nil, fmt.Errorf("%w: class name %q, want %s", ErrClassSet, spec.Name, classNamePattern)
	}
	if spec.MaxRetries < 0 || spec.MaxRetries > MaxRetries {
		return nil, fmt.Errorf("%w: class %s retries %d, want 0 to %d", ErrClassSet, spec.Name, spec.MaxRetries, MaxRetries)
	}
	if fallback != (len(spec.Signatures) == 0) {
		return nil, fmt.Errorf("%w: class %s: only the fallback class has no signatures", ErrClassSet, spec.Name)
	}
	if len(spec.Signatures) > MaxSignatures {
		return nil, fmt.Errorf("%w: class %s has %d signatures, want at most %d", ErrClassSet, spec.Name, len(spec.Signatures), MaxSignatures)
	}
	class := &deadLetterClass{spec: spec, queue: make(chan Failure, capacity)}
	for _, signature := range spec.Signatures {
		if len(signature) > MaxSignatureLen {
			return nil, fmt.Errorf("%w: class %s: signature of %d bytes, want at most %d", ErrClassSet, spec.Name, len(signature), MaxSignatureLen)
		}
		re, err := regexp.Compile("(?m)" + signature)
		if err != nil {
			return nil, fmt.Errorf("%w: class %s: %w", ErrClassSet, spec.Name, err)
		}
		class.signatures = append(class.signatures, re)
	}
	return class, nil
}

// Route classifies f. When its class grants another attempt the verdict says retry and
// nothing is parked; otherwise f waits in the class queue, and a full queue refuses it with
// ErrFull, which is retryable.
func (d *DeadLetters) Route(ctx context.Context, f Failure) (Verdict, error) {
	if err := ctx.Err(); err != nil {
		return Verdict{}, fmt.Errorf("broker: route a failure of %s: %w", f.Unit, err)
	}
	class := d.classify(f.Text)
	verdict := Verdict{Class: class.spec.Name, Retry: f.Attempt <= class.spec.MaxRetries}
	if verdict.Retry {
		return verdict, nil
	}
	select {
	case class.queue <- f:
		return verdict, nil
	default:
		return verdict, fmt.Errorf("%w: dead-letter class %s, capacity %d", ErrFull, class.spec.Name, cap(class.queue))
	}
}

func (d *DeadLetters) classify(text string) *deadLetterClass {
	for _, class := range d.classes {
		for _, re := range class.signatures {
			if re.MatchString(text) {
				return class
			}
		}
	}
	return d.fallback
}

// Next returns the oldest failure parked in class, waiting for one until ctx ends.
func (d *DeadLetters) Next(ctx context.Context, class string) (Failure, error) {
	c := d.byName[class]
	if c == nil {
		return Failure{}, fmt.Errorf("%w: %s", ErrUnknownClass, class)
	}
	select {
	case f := <-c.queue:
		return f, nil
	case <-ctx.Done():
		return Failure{}, fmt.Errorf("broker: wait for a failure in %s: %w", class, ctx.Err())
	}
}

// Len returns the number of failures parked in class; an unknown class holds none.
func (d *DeadLetters) Len(class string) int {
	if c := d.byName[class]; c != nil {
		return len(c.queue)
	}
	return 0
}
