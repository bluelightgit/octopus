'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import { useTranslations } from 'next-intl';
import { Monitor, Globe, Clock, Shield, HelpCircle, X } from 'lucide-react';
import { Input } from '@/components/ui/input';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { useSettingList, useSetSetting, SettingKey } from '@/api/endpoints/setting';
import { toast } from '@/components/common/Toast';
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/animate-ui/components/animate/tooltip';

export function SettingSystem() {
    const t = useTranslations('setting');
    const { data: settings } = useSettingList();
    const setSetting = useSetSetting();

    const [proxyUrl, setProxyUrl] = useState('');
    const [statsSaveInterval, setStatsSaveInterval] = useState('');
    const [relayUpstreamHeaderTimeout, setRelayUpstreamHeaderTimeout] = useState('');
    const [relayNonStreamTimeout, setRelayNonStreamTimeout] = useState('');
    const [relayStreamIdleTimeout, setRelayStreamIdleTimeout] = useState('');
    const [relayResponsesPreludeTimeout, setRelayResponsesPreludeTimeout] = useState('');
    const [corsAllowOrigins, setCorsAllowOrigins] = useState('');
    const [corsInputValue, setCorsInputValue] = useState('');

    const initialValues = useRef<Record<string, string>>({});

    useEffect(() => {
        if (!settings) return;

        const getSettingValue = (key: string) => settings.find(s => s.key === key)?.value ?? '';

        const nextProxyUrl = getSettingValue(SettingKey.ProxyURL);
        const nextStatsSaveInterval = getSettingValue(SettingKey.StatsSaveInterval);
        const nextRelayUpstreamHeaderTimeout = getSettingValue(SettingKey.RelayUpstreamHeaderTimeout);
        const nextRelayNonStreamTimeout = getSettingValue(SettingKey.RelayNonStreamTimeout);
        const nextRelayStreamIdleTimeout = getSettingValue(SettingKey.RelayStreamIdleTimeout);
        const nextRelayResponsesPreludeTimeout = getSettingValue(SettingKey.RelayResponsesPreludeTimeout);
        const nextCorsAllowOrigins = getSettingValue(SettingKey.CORSAllowOrigins);

        queueMicrotask(() => {
            setProxyUrl(nextProxyUrl);
            setStatsSaveInterval(nextStatsSaveInterval);
            setRelayUpstreamHeaderTimeout(nextRelayUpstreamHeaderTimeout);
            setRelayNonStreamTimeout(nextRelayNonStreamTimeout);
            setRelayStreamIdleTimeout(nextRelayStreamIdleTimeout);
            setRelayResponsesPreludeTimeout(nextRelayResponsesPreludeTimeout);
            setCorsAllowOrigins(nextCorsAllowOrigins);
        });

        initialValues.current[SettingKey.ProxyURL] = nextProxyUrl;
        initialValues.current[SettingKey.StatsSaveInterval] = nextStatsSaveInterval;
        initialValues.current[SettingKey.RelayUpstreamHeaderTimeout] = nextRelayUpstreamHeaderTimeout;
        initialValues.current[SettingKey.RelayNonStreamTimeout] = nextRelayNonStreamTimeout;
        initialValues.current[SettingKey.RelayStreamIdleTimeout] = nextRelayStreamIdleTimeout;
        initialValues.current[SettingKey.RelayResponsesPreludeTimeout] = nextRelayResponsesPreludeTimeout;
        initialValues.current[SettingKey.CORSAllowOrigins] = nextCorsAllowOrigins;
    }, [settings]);

    const handleSave = (key: string, value: string) => {
        const initialValue = initialValues.current[key] ?? '';
        if (value === initialValue) return;

        setSetting.mutate({ key, value }, {
            onSuccess: () => {
                toast.success(t('saved'));
                initialValues.current[key] = value;
            }
        });
    };

    const relayTimeoutFields = [
        {
            key: SettingKey.RelayUpstreamHeaderTimeout,
            label: t('relayUpstreamHeaderTimeout.label'),
            placeholder: t('relayUpstreamHeaderTimeout.placeholder'),
            value: relayUpstreamHeaderTimeout,
            setValue: setRelayUpstreamHeaderTimeout,
        },
        {
            key: SettingKey.RelayNonStreamTimeout,
            label: t('relayNonStreamTimeout.label'),
            placeholder: t('relayNonStreamTimeout.placeholder'),
            value: relayNonStreamTimeout,
            setValue: setRelayNonStreamTimeout,
        },
        {
            key: SettingKey.RelayStreamIdleTimeout,
            label: t('relayStreamIdleTimeout.label'),
            placeholder: t('relayStreamIdleTimeout.placeholder'),
            value: relayStreamIdleTimeout,
            setValue: setRelayStreamIdleTimeout,
        },
        {
            key: SettingKey.RelayResponsesPreludeTimeout,
            label: t('relayResponsesPreludeTimeout.label'),
            placeholder: t('relayResponsesPreludeTimeout.placeholder'),
            hint: t('relayResponsesPreludeTimeout.hint'),
            value: relayResponsesPreludeTimeout,
            setValue: setRelayResponsesPreludeTimeout,
        },
    ];

    const corsAllowOriginsList = useMemo(() => {
        const value = corsAllowOrigins.trim();
        if (!value) return [];
        if (value === '*') return ['*'];
        return Array.from(new Set(
            value
                .split(/[,\n，]/)
                .map(item => item.trim())
                .filter(Boolean)
        ));
    }, [corsAllowOrigins]);

    const corsAllowOriginsDisplay = useMemo(
        () => (corsAllowOriginsList.length > 0 ? corsAllowOriginsList.join(', ') : t('corsAllowOrigins.hint')),
        [corsAllowOriginsList, t]
    );

    const saveCorsAllowOrigins = (origins: string[]) => {
        const normalizedOrigins = Array.from(new Set(
            origins
                .map(origin => origin.trim())
                .filter(Boolean)
        ));
        const normalizedValue = normalizedOrigins.includes('*') ? '*' : normalizedOrigins.join(',');
        setCorsAllowOrigins(normalizedValue);
        handleSave(SettingKey.CORSAllowOrigins, normalizedValue);
    };

    const handleAddCorsOrigin = () => {
        const newOrigins = Array.from(new Set(
            corsInputValue
                .split(/[,\n，]/)
                .map(item => item.trim())
                .filter(Boolean)
        ));
        if (newOrigins.length === 0) return;

        if (newOrigins.includes('*')) {
            saveCorsAllowOrigins(['*']);
            setCorsInputValue('');
            return;
        }

        const base = corsAllowOriginsList.includes('*') ? [] : corsAllowOriginsList;
        const merged = Array.from(new Set([...base, ...newOrigins]));
        saveCorsAllowOrigins(merged);
        setCorsInputValue('');
    };

    const handleRemoveCorsOrigin = (originToRemove: string) => {
        const nextOrigins = corsAllowOriginsList.filter(origin => origin !== originToRemove);
        saveCorsAllowOrigins(nextOrigins);
    };

    return (
        <div className="rounded-3xl border border-border bg-card p-6 space-y-5">
            <h2 className="text-lg font-bold text-card-foreground flex items-center gap-2">
                <Monitor className="h-5 w-5" />
                {t('system')}
            </h2>

            {/* 代理地址 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Globe className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('proxyUrl.label')}</span>
                </div>
                <Input
                    value={proxyUrl}
                    onChange={(e) => setProxyUrl(e.target.value)}
                    onBlur={() => handleSave(SettingKey.ProxyURL, proxyUrl)}
                    placeholder={t('proxyUrl.placeholder')}
                    className="w-48 rounded-xl"
                />
            </div>

            {/* 统计保存周期 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Clock className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('statsSaveInterval.label')}</span>
                </div>
                <Input
                    type="number"
                    value={statsSaveInterval}
                    onChange={(e) => setStatsSaveInterval(e.target.value)}
                    onBlur={() => handleSave(SettingKey.StatsSaveInterval, statsSaveInterval)}
                    placeholder={t('statsSaveInterval.placeholder')}
                    className="w-48 rounded-xl"
                />
            </div>

            {relayTimeoutFields.map((field) => (
                <div key={field.key} className="flex items-center justify-between gap-4">
                    <div className="flex items-center gap-3">
                        <Clock className="h-5 w-5 text-muted-foreground" />
                        <span className="text-sm font-medium">{field.label}</span>
                        {field.hint && (
                            <TooltipProvider>
                                <Tooltip>
                                    <TooltipTrigger asChild>
                                        <HelpCircle className="size-4 cursor-help text-muted-foreground" />
                                    </TooltipTrigger>
                                    <TooltipContent>
                                        {field.hint}
                                    </TooltipContent>
                                </Tooltip>
                            </TooltipProvider>
                        )}
                    </div>
                    <Input
                        type="number"
                        value={field.value}
                        onChange={(e) => field.setValue(e.target.value)}
                        onBlur={() => handleSave(field.key, field.value)}
                        placeholder={field.placeholder}
                        className="w-48 rounded-xl"
                    />
                </div>
            ))}

            {/* CORS 跨域白名单 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Shield className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('corsAllowOrigins.label')}</span>
                    <TooltipProvider>
                        <Tooltip>
                            <TooltipTrigger asChild>
                                <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                            </TooltipTrigger>
                            <TooltipContent>
                                {t('corsAllowOrigins.hint')}
                                <br />
                                {t('corsAllowOrigins.example')}
                            </TooltipContent>
                        </Tooltip>
                    </TooltipProvider>
                </div>
                <Popover>
                    <PopoverTrigger asChild>
                        <button
                            type="button"
                            className="border-input focus-visible:border-ring focus-visible:ring-ring/50 w-48 min-h-9 rounded-xl border bg-transparent px-3 py-2 text-left text-sm shadow-xs transition-[color,box-shadow] outline-none focus-visible:ring-[3px]"
                            title={corsAllowOriginsDisplay}
                        >
                            <span className={`block overflow-hidden text-ellipsis whitespace-nowrap ${corsAllowOriginsList.length === 0 ? 'text-muted-foreground' : ''}`}>
                                {corsAllowOriginsDisplay}
                            </span>
                        </button>
                    </PopoverTrigger>
                    <PopoverContent className="w-72 space-y-2 rounded-3xl p-3 bg-card">
                        <Input
                            value={corsInputValue}
                            onChange={(e) => setCorsInputValue(e.target.value)}
                            onKeyDown={(e) => {
                                if (e.key === 'Enter') {
                                    e.preventDefault();
                                    handleAddCorsOrigin();
                                }
                            }}
                            placeholder={t('corsAllowOrigins.example')}
                            className="h-9 rounded-xl"
                            autoFocus
                        />
                        <div className="max-h-48 space-y-1 overflow-y-auto">
                            {corsAllowOriginsList.length > 0 && (
                                corsAllowOriginsList.map((origin) => (
                                    <div key={origin} className="flex items-center justify-between gap-2 rounded-xl border border-border/60 px-2 py-1">
                                        <span className="break-all text-xs leading-5">{origin}</span>
                                        <button
                                            type="button"
                                            onClick={() => handleRemoveCorsOrigin(origin)}
                                            className="text-muted-foreground transition-colors hover:text-destructive"
                                            aria-label={`remove ${origin}`}
                                        >
                                            <X className="size-4" />
                                        </button>
                                    </div>
                                ))
                            )}
                        </div>
                    </PopoverContent>
                </Popover>
            </div>
        </div>
    );
}

