package main

import "errors"

func runConfirmedAction[T any](confirm func() (bool, error), action func() (T, error)) (T, bool, error) {
	var zero T
	if confirm == nil || action == nil {
		return zero, false, errors.New("confirmed action is not configured")
	}
	confirmed, err := confirm()
	if err != nil || !confirmed {
		return zero, false, err
	}
	result, err := action()
	return result, true, err
}
