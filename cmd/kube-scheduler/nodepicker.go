package main

import (
	"context"

	v1 "k8s.io/api/core/v1"
)

type NodePickerArgs map[string]string

type NodePicker interface {
	Name() string
	Pick(ctx context.Context, pod *v1.Pod, nodes []*v1.Node, args NodePickerArgs) string
}
