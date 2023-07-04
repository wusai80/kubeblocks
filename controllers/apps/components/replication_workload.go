/*
Copyright (C) 2022-2023 ApeCloud Co., Ltd

This file is part of KubeBlocks project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package components

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/apecloud/kubeblocks/internal/constant"
	"github.com/apecloud/kubeblocks/internal/controller/builder"
)

type replicationComponentWorkloadBuilder struct {
	componentWorkloadBuilderBase
}

var _ componentWorkloadBuilder = &replicationComponentWorkloadBuilder{}

func (b *replicationComponentWorkloadBuilder) BuildWorkload() componentWorkloadBuilder {
	return b.BuildWorkload4StatefulSet("replication")
}

func (b *replicationComponentWorkloadBuilder) BuildService() componentWorkloadBuilder {
	buildFn := func() ([]client.Object, error) {
		svcList, err := builder.BuildSvcList(b.Comp.GetCluster(), b.Comp.GetSynthesizedComponent())
		if err != nil {
			return nil, err
		}
		objs := make([]client.Object, 0, len(svcList))
		for _, svc := range svcList {
			svc.Spec.Selector[constant.RoleLabelKey] = constant.Primary
			objs = append(objs, svc)
		}
		return objs, err
	}
	return b.BuildWrapper(buildFn)
}