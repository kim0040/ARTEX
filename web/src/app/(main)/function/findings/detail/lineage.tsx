"use client";

import * as React from "react";

import { ExplorationGraph } from "@/components/exploration-graph";
import { api } from "@/lib/api";
import type { Edge, TaskNode } from "@/lib/types";

// FindingLineageView는 탐색 그래프에서 작업의 시작부터
// 노드에서 이 발견의 노드까지 내려감 — 작업 그래프와 같은 공격 경로 그림 캔버스, （탐색 그래프는 작업이 어디까지 이어졌는지 보여주는 그림입니다）
// 이 발견이 나온 경로만 잘라 보여 줍니다.
export function FindingLineageView({ findingId }: { findingId: string }) {
  const [nodes, setNodes] = React.useState<TaskNode[]>([]);
  const [edges, setEdges] = React.useState<Edge[]>([]);
  const [loaded, setLoaded] = React.useState(false);

  React.useEffect(() => {
    let alive = true;
    api
      .findingLineage(findingId)
      .then((g) => {
        if (!alive) return;
        setNodes(g.nodes ?? []);
        setEdges(g.edges ?? []);
      })
      .catch(() => {})
      .finally(() => {
        if (alive) setLoaded(true);
      });
    return () => {
      alive = false;
    };
  }, [findingId]);

  if (loaded && nodes.length === 0) {
    return (
      <p className="text-muted-foreground p-6 text-sm">
        보여줄 경로가 없습니다(이 발견에 탐색 노드가 없거나, 속한 작업이 삭제되었습니다).
      </p>
    );
  }

  return (
    <ExplorationGraph
      nodes={nodes}
      edges={edges}
      className="h-[68vh]"
      emptyHint={loaded ? "경로 없음" : "불러오는 중…"}
    />
  );
}
