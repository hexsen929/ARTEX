"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import { RadioTowerIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { api } from "@/lib/api";
import type { Settings } from "@/lib/types";

type CallbackAddressSettings = {
  address: string;
  source: NonNullable<Settings["callback_addr_source"]>;
  locked: boolean;
  error: string;
};

const sourceLabels: Record<CallbackAddressSettings["source"], string> = {
  env: "环境变量",
  settings: "后台设置",
  config: "配置文件",
  unset: "未配置",
};

function callbackAddressSettings(settings: Settings): CallbackAddressSettings {
  const source = settings.callback_addr_source;
  if (
    typeof settings.callback_addr !== "string" ||
    !source ||
    !Object.hasOwn(sourceLabels, source) ||
    typeof settings.callback_addr_locked !== "boolean"
  ) {
    throw new Error("服务未返回完整的回连设置，请确认后端版本后重试");
  }
  return {
    address: settings.callback_addr,
    source,
    locked: settings.callback_addr_locked,
    error: settings.callback_addr_error ?? "",
  };
}

export function CallbackAddressCard() {
  const [saved, setSaved] = useState<CallbackAddressSettings | null>(null);
  const [input, setInput] = useState("");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const mounted = useRef(false);
  const generation = useRef(0);
  const savingRef = useRef(false);

  // Keep this form independent of the page's apply() callback: saving another
  // setting must not replace an unsaved callback address with a stale response.
  const load = useCallback(async () => {
    const current = ++generation.current;
    setLoading(true);
    setError("");
    try {
      const settings = callbackAddressSettings(await api.settings());
      if (!mounted.current || generation.current !== current) return;
      setSaved(settings);
      setInput(settings.address);
    } catch (e) {
      if (!mounted.current || generation.current !== current) return;
      setError(`读取失败：${(e as Error).message}`);
    } finally {
      if (mounted.current && generation.current === current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    mounted.current = true;
    void load();
    return () => {
      mounted.current = false;
      generation.current += 1;
    };
  }, [load]);

  const persist = async (address: string) => {
    if (!saved || loading || saved.locked || savingRef.current) return;
    savingRef.current = true;
    setSaving(true);
    setError("");
    const current = ++generation.current;
    try {
      const settings = callbackAddressSettings(await api.setSettings({ callback_addr: address.trim() }));
      if (!mounted.current || generation.current !== current) return;
      setSaved(settings);
      setInput(settings.address);
      toast.success(
        settings.address
          ? "回连地址已保存，对新建连接立即生效"
          : "回连地址已清空，已有连接保持原配置",
      );
    } catch (e) {
      if (!mounted.current || generation.current !== current) return;
      setError(`保存失败：${(e as Error).message}`);
    } finally {
      savingRef.current = false;
      if (mounted.current && generation.current === current) setSaving(false);
    }
  };

  const editable = saved !== null && !loading && !saving && !saved.locked;
  const dirty = saved !== null && input.trim() !== saved.address;

  return (
    <Card className="mb-4 min-w-0 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <RadioTowerIcon className="size-4 shrink-0" />
          回连地址
        </CardTitle>
        <CardDescription>
          指定目标主动连接 ARTEX 时使用的 IP 或域名。普通扫描、HTTP 探测和爬虫可保持留空。
        </CardDescription>
      </CardHeader>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          void persist(input);
        }}
      >
        <CardContent className="min-w-0">
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="callback-address">回连主机地址</FieldLabel>
              <Input
                id="callback-address"
                autoComplete="off"
                autoCapitalize="off"
                spellCheck={false}
                className="min-w-0 font-mono text-sm"
                placeholder="目标能访问的 IP 或域名（留空=不配置）"
                value={input}
                disabled={!editable}
                onChange={(event) => setInput(event.target.value)}
                aria-describedby="callback-address-help callback-address-effect"
                aria-invalid={error || saved?.error ? true : undefined}
              />
              <FieldDescription id="callback-address-help">
                填写主机地址，不填写管理页面网址、协议或路径。兼容带端口的地址；实际监听端口由具体会话或隧道决定。
              </FieldDescription>
            </Field>
            <div role="status" aria-live="polite" className="text-muted-foreground min-w-0 text-xs">
              {saved ? (
                <>
                  <p className="break-all">
                    {saved.address ? (
                      <>
                        {saved.error ? "当前配置（待修正）：" : "当前生效："}
                        <code className="font-mono">{saved.address}</code>
                      </>
                    ) : (
                      "当前未配置回连地址"
                    )}
                  </p>
                  <p>来源：{sourceLabels[saved.source]}</p>
                </>
              ) : loading ? (
                "正在读取回连设置…"
              ) : null}
            </div>
            {saved?.error ? (
              <p role="alert" className="text-destructive break-words text-sm">
                {saved.error}。{saved.locked ? "请修正或移除启动环境变量后重启服务。" : "请修正地址，或清空并保存；当前地址不会用于新建连接。"}
              </p>
            ) : null}
            {saved?.locked ? (
              <FieldDescription>
                当前由环境变量 ARTEX_CALLBACK_ADDR 控制，后台只读。移除启动环境变量并重启服务后，可在这里保存地址。
              </FieldDescription>
            ) : null}
            <FieldDescription id="callback-address-effect">
              保存或清空后，对之后新建的连接立即生效，无需重启。已有会话和隧道保持原配置；已有隧道的自动重连也沿用原计划，需要主动重建才会换地址。
            </FieldDescription>
            <FieldDescription>
              保存地址不会启动监听、重建连接、修改路由或自动建立公网映射；目标是否可达仍取决于实际网络。
            </FieldDescription>
            {error ? (
              <p role="alert" className="text-destructive break-words text-sm">
                {error}
              </p>
            ) : null}
          </FieldGroup>
        </CardContent>
        <CardFooter className="mt-4 flex-wrap gap-2">
          <Button type="submit" disabled={!editable || !dirty}>
            {saving ? "保存中…" : "保存回连地址"}
          </Button>
          <Button
            type="button"
            variant="outline"
            disabled={!editable || (!saved?.address && !input.trim())}
            onClick={() => void persist("")}
          >
            清空并保存
          </Button>
          {!saved && error ? (
            <Button type="button" variant="outline" disabled={loading} onClick={() => void load()}>
              重新加载
            </Button>
          ) : null}
        </CardFooter>
      </form>
    </Card>
  );
}
