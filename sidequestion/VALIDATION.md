# `/btw` 검증 기록

초보 안내: 이 문서는 곁길 질문 기능을 언제, 어떤 검사로 확인했는지를 적은 기록입니다. 사용 절차를 새로 만들지 않습니다.

날짜: 2026-09-10. 브랜치: `codex/btw-side-question`. 기준: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

독립 PostgreSQL 테스트 저장소와 데이터 디렉터리를 썼습니다. 실제 모델 자격 증명은 독립 테스트 환경에만 넣었고, 코드나 이 기록에는 쓰지 않았으며, 제품 기본 모델도 바꾸지 않았습니다. Go 1.26.3, norma v0.3.6, Next.js 16.2.9.

실제 모델 대화, 반환 객체, 공학 단언, Qwen 원문 심사 텍스트는 [validation-2026-09-10.json](validation-2026-09-10.json)에 있습니다. API 자격 증명은 없습니다.

## 공학 검사

| 범위 | 결과 | 증거 |
| --- | --- | --- |
| 구조화 메시지, 도구 인자 깊은 복사 | 통과 | `TestCheckpointDeepCopyAndBoundaries` |
| 요약 / 압축 요청은 덮지 않음, 완전한 답과 종료 상태 발행, 반쪽 답 제외 | 통과 | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| 실제 모델 풀 구성원 신원 | 통과 | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| 도구 짝, 20조 재생, 예산 자르기, 한도 초과 오류 | 통과 | `TestBuildRequestCompactionToolPairingAndBudget` |
| 메인과 곁길 병렬, 양방향 취소 격리 | 통과 | 블로킹 Provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| 도구 실행 없음, 스트리밍 / 비스트리밍, 실패 때 이미 받은 사용량 | 통과 | `TestServiceNoToolsAndUsageOnFailure` |
| 실제 norma ChatAgent + 로컬 Read 도구, 메인 transcript / 활동 격리 | 통과 | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`, 스트리밍과 비스트리밍 하위 사례 |
| 저장, 페이지, 멱등, 재시작 후 부분 답 유지 | 통과 | `TestSideHistoryIdempotencyPagingAndRecovery` |
| 비우기와 늦은 쓰기 경합, 부모 자원 삭제, 버전 비교 | 통과 | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent / Worker 아카이브와 복구, v1/v2/v3 | 통과 | `TestSideTaskArchiveVersions` |
| 부모 인터페이스 세 가지, 인증, 자원 소속, Worker 논리 삭제 | 통과 | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| 바쁜 메인 세션도 곁길 가능, 독립 SSE 재연결 / 끊김, 취소, 비우기 | 통과 | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 부모 세션당 1 / 전역 4 동시 | 통과 | `TestSideHTTP…` 사례 둘 |
| 제출 전 스냅샷 저장, 재시작 후 이어 묻기, 옛 세션은 스냅샷을 위조할 수 없음 | 통과 | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| 캐시의 설정이 지워지거나 모델이 바뀌면 계속을 거절 | 통과 | `TestSideRejectsDeletedOrChangedCachedProfile` |
| 아카이브 전에 취소하고 최종 답과 사용량 저장을 기다림 | 통과 | `TestSideTaskDrainPersistsBeforeArchive` |
| 스트리밍 소비자가 일찍 취소하면 사용량을 한 번만 기록하고 곁길에 귀속 | 통과 | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| 재시작으로 복구된 Worker / deadline 실행 맥락이 새 스냅샷을 계속 발행 | 통과 | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| 관련 패키지 race 검사 | 통과 | 아래 명령 |
| TypeScript 와 프로덕션 빌드 | 통과 | `npx tsc --noEmit`, `npm run build` |
| 추가한 프론트 모듈 Biome | 통과 | `biome check`, 새 모듈 3개 |

버릴 수 있는 데이터베이스에 `ARTEX_PG_DSN` 을 넣은 뒤 자동 검사를 재현할 수 있습니다(프로덕션 저장소를 가리키지 마세요).

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

전체 Go 회귀는 모두 초록이 아니었습니다. `server` 패키지의 기존 테스트 둘이 임시 디렉터리 정리 단계에서 실패했고, 둘 다 `TempDir RemoveAll … directory not empty` 를 보고했습니다.

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

위 기준 커밋에서 소스를 받은 뒤 같은 격리 환경에서 `server` 패키지를 다시 돌려도 이 정리 실패가 재현되었습니다. 기준 실행에서는 `TestCoreTaskLifecyclePG` 의 목표 노드 수 단언 실패도 있었습니다. 최종 수정 뒤 `server` 회귀에는 그 단언 실패가 없었습니다. 다른 패키지는 통과했고, 이번 곁길 관련 사례와 race 검사도 통과했습니다. 기준선의 문제를 이번 인수 통과로 적지 않았고, 문제를 숨기려고 기존 단언을 고치지도 않았습니다.

Next.js 빌드에는 원래 있던 lockfile 여러 개 / workspace root 추론 경고가 있습니다. 빌드는 끝났고 모든 페이지가 생성되었습니다.

## 브라우저 검사

Codex 인앱 브라우저로 독립 로컬 Go 서비스와 Next.js 개발 서버에 연결했습니다. 데스크톱과 390 × 844 좁은 화면에서 아래 수동 자동 조작을 하고, 스크린샷과 브라우저 로그를 확인했습니다.

- 일반 채팅이 도는 동안 `/btw` 를 입력하면 메인 내용과 곁길이 함께 보입니다. 데스크톱 옆 패널은 정상입니다.
- 이어서 질문합니다. 곁길을 멈추면 이미 만든 부분이 남고, 메인 흐름은 계속됩니다.
- 패널을 닫아도 요청은 계속되고, 다시 열면 끝난 답이 돌아옵니다. 페이지를 새로고친 뒤 빈 `/btw` 가 기록을 복구합니다.
- 좁은 화면 Drawer 의 입력, 버튼, 기록, 닫기가 정상이고 가로로 넘치지 않습니다.
- 비우기는 확인 창을 씁니다. 비운 뒤 기록은 사라지고, 메인 transcript 와 스냅샷은 남습니다.
- 작업 MainAgent 와 Worker 둘에서 각각 질문하고 전환했습니다. 에이전트 라벨과 기록이 섞이지 않았습니다.
- 블로킹 로컬 모델 픽스처가 Worker 를 실행 중으로 유지합니다. Worker 메인 입력칸에서 `/btw` 를 제출하고 곁길을 멈춘 뒤에도 Worker 는 실시간 실행과 자신의 일시정지 버튼을 보여 주고, 곁길은 부분 답을 저장합니다.
- 브라우저 오류 / 경고 로그는 비어 있습니다.

통제된 픽스처는 동시 타이밍을 정확히 확인하기 위한 것이며, 실제 모델의 출력 속도에 의존하지 않습니다. 디버그 중 Worker 실행 검사 두 번은 유효한 동시 창을 만들지 못했습니다(작업이 이미 끝났거나 답이 일찍 끝남). 픽스처를 고친 뒤 다시 해서 통과했습니다. 그 처음 조작은 유효한 통과로 적지 않습니다.

## 실제 모델 대화

`grok-4.6` 을 먼저 살폈습니다. OpenAI 호환 인터페이스 `http://127.0.0.1:12580/tingly/openai`. 탐색 HTTP 200, 반환 모델 이름 `grok-4.6` 과 `READY`, 2.82초. 첫 선택이 가능해서 Tingly `glm` 이나 지푸 `glm-5.3` 예비 사슬은 켜지 않았습니다. 그 예비 서비스 둘은 이번에 검증하지 않았습니다.

| 장면 | 실제 결과 |
| --- | --- |
| 메인 세션이 도는 동안 자산, 목표, 표식을 물음 | `redhaze.top`, 첫 페이지 읽기와 요약 목표, `BTW-REAL-0910` 을 반환. 곁길 완료, 16.97초 |
| 메인 세션이 첫 페이지를 읽은 뒤 도구 근거를 물음 | WebFetch 200, curl 이동 301 → 302 → 200, 페이지 제목을 올바르게 인용. 7.24초 |
| 곁길이 Bash 로 테스트 파일을 만들라고 함 | 실행을 거절, 대상 파일은 만들어지지 않음. 7.74초 |
| 끝난 뒤의 곁길이 메인 맥락을 바꾸지 않음 | 메인 transcript SHA-256 과 메인 활동 기록이 일치. 곁길 도구 실행 횟수 0 |
| Go 서비스를 정말로 멈추고 재시작한 뒤 이어 물음 | 이전 곁길 기록 3건을 유지. 저장된 스냅샷에서 자산, 표식, 제목을 바로 답함. 메인 에이전트를 다시 돌리지 않음 |
| 새 세션이 Grok 비스트리밍 설정을 씀 | 자산과 `ATOMIC-0910` 을 올바르게 답함. 반환되고 저장된 사용량: input 11734, output 138, cache_read 11520 |

자산 사례의 메인 세션은 WebFetch 와 Bash/curl 로 공개 첫 페이지를 읽었습니다. 랜딩 페이지는 `https://id.redhaze.top/home` 이고, 제목에는 RedHaze Group 과 중국어 회사 상호가 들어 있었습니다. Bash 는 응답을 로컬 테스트 파일에 잠시 저장했습니다. 원격으로 쓰기는 하지 않았습니다. 이 사실은 「곁길이 도구를 실행하지 않았다」와 따로 확인했습니다.

메인 transcript 검사값: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**사용량 제한:** Tingly 의 Grok 스트리밍 응답은 usage 를 반환하지 않았습니다. 따로 `stream_options.include_usage=true` 를 보내 확인했고, HTTP 200, 데이터 프레임 12개, usage 프레임 0개였습니다. 그래서 스트리밍 테스트의 0 은 엔드포인트가 사용량을 주지 않았다는 뜻이지, 과금이 없다는 뜻이 아닙니다. 비스트리밍 사용량과 픽스처의 실패 / 취소 사용량은 올바르게 저장되었습니다.

## Qwen 심사

심사 모델 `qwen-flash`, OpenAI 호환 인터페이스 `https://dashscope.aliyuncs.com/compatible-mode/v1`, HTTP 200. 앞의 실제 곁길 대화 세 건, 메인 세션 도구 근거, 공학 단언을 제공했습니다. 반환은 `verdict: accept`, `concerns: []` 였고, 답이 자산, 표식, 페이지 읽기 증거와 일치하며 곁길의 도구 거절이 제약에 맞는다고 보았습니다. 심사 사용량: prompt 6625, completion 312, total 6937.

이번 Qwen 심사 범위에는 나중에 추가한 서비스 재시작과 비스트리밍 테스트가 없습니다. Qwen 의 「쓰기 없음」 요약은 너무 넓습니다. 메인 세션 curl 은 실제로 로컬 응답 임시 파일을 만들었고, 위에 분명히 적혀 있습니다. 동시성, 도구 실행 0, transcript 격리는 공학 단언이 판단하고, 모델 심사는 답의 품질을 돕는 평가일 뿐입니다.
