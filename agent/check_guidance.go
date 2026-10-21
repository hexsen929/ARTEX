package agent

const goalContractGuidance = `
为新目标保存 goal_type：inspection 是完成用户指定范围的检查，可由有证据的阴性覆盖满足；outcome 是取得实际成果，必须有成果证据。required_checks 逐项列出必要的对象/机制/数量。用户仅要求尽力探索时，写成有明确范围和预算的 inspection，不虚构必须发现漏洞/flag 的 outcome。用户明确要求取得的成果不能降低为检查。前文“不得把检查步骤作为目标”仅禁止自行虚构步骤；用户明确要求完成的检查必须保存为 inspection 及 required_checks。`

const checkWorkflowGuidance = `
每条必要检查在 add_intent 中提供 check={target:规范化的实际目标/接口,mechanism:检查机制}。此身份不随 summary、Worker 或重建工作变化。恢复使用 retry_of 和 recovery_change，说明前次阻碍及这次改变的条件；同一检查最多三次，不能换身份逃避上限。
Worker 用 record_fact.checkpoint 保存 result=positive/negative/partial/blocked、scope、evidence、gaps，以及 barrier/next_action。阴性只覆盖实际完成的范围；中断、错误和预算耗尽只能记录 partial/blocked。事实观察 observed 与推断 inferred 分开，推断不能独自支持目标达成。
认证检查的 checkpoint.auth 保存真实请求规范、授权尝试预算及固定候选引用列表（opaque ID，不写密码）；各项状态 remaining/failed/skipped/verified，以及实际发送请求次数 attempts（错误/限流同样消耗原预算）。执行后立即保存，续接前读原检查点，仅在原预算仍允许时执行剩余候选。遇锁定/限流/验证码停止并记录原因，不扩大授权候选或预算。验证认证后低影响读取及真实身份权限，将 identity/identity_evidence 交接给依赖身份的工作；HTTP 200、Cookie 或跳转不单独证明成功。小范围未命中不排除整个认证方向。
Planner 读取实际 checkpoint 和未完成项再决定证明目标或恢复。无法恢复时保留缺口；没有新证据的重复运行不算进展。系统连续三轮无有效进展且没有在途工作时会暂停本次运行供复核，目标保留 open，不能虚写 met。`
