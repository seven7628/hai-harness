import React from 'react';
import {Video} from '@remotion/media';
import {
  AbsoluteFill,
  Easing,
  Sequence,
  interpolate,
  staticFile,
  useCurrentFrame,
  useVideoConfig,
} from 'remotion';

const RED = '#c23a2e';
const CREAM = '#f4efe4';
const INK = '#191816';
const FPS = 30;

const clamp = {
  extrapolateLeft: 'clamp' as const,
  extrapolateRight: 'clamp' as const,
};

const enter = (frame: number, fps: number, delay = 0) =>
  interpolate(frame, [delay, delay + 0.55 * fps], [0, 1], {
    ...clamp,
    easing: Easing.bezier(0.16, 1, 0.3, 1),
  });

const leave = (frame: number, fps: number, start: number, end: number) =>
  interpolate(frame, [start, end], [1, 0], {
    ...clamp,
    easing: Easing.bezier(0.7, 0, 0.84, 0),
  });

const Caption: React.FC<{
  children: React.ReactNode;
  start: number;
  end: number;
  top?: number;
  small?: boolean;
}> = ({children, start, end, top = 1390, small = false}) => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  const opacity = Math.min(
    enter(frame, fps, start),
    leave(frame, fps, end - 0.45 * fps, end),
  );
  const translate = interpolate(frame, [start, start + 0.55 * fps], [28, 0], {
    ...clamp,
    easing: Easing.bezier(0.16, 1, 0.3, 1),
  });

  return (
    <div
      style={{
        position: 'absolute',
        left: 72,
        right: 72,
        top,
        opacity,
        translate: `0px ${translate}px`,
        color: CREAM,
        fontSize: small ? 36 : 48,
        fontWeight: 800,
        lineHeight: 1.18,
        letterSpacing: -1.2,
        textAlign: 'center',
        textShadow: '0 4px 22px rgba(0,0,0,0.6)',
      }}
    >
      {children}
    </div>
  );
};

const Pill: React.FC<{children: React.ReactNode; top: number}> = ({children, top}) => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  const opacity = interpolate(frame, [0.2 * fps, 0.75 * fps], [0, 1], clamp);
  return (
    <div
      style={{
        position: 'absolute',
        top,
        left: 72,
        padding: '14px 24px',
        borderRadius: 999,
        background: RED,
        color: '#fff9ef',
        fontSize: 26,
        fontWeight: 800,
        letterSpacing: 1.2,
        opacity,
      }}
    >
      {children}
    </div>
  );
};

const SourceVideo: React.FC = () => {
  return (
    <>
      <AbsoluteFill style={{backgroundColor: INK}}>
        <Video
          src={staticFile('hai-promo.mp4')}
          objectFit="cover"
          style={{
            width: '100%',
            height: '100%',
            scale: 1.12,
            filter: 'blur(22px) brightness(0.36) saturate(0.75)',
            opacity: 0.9,
          }}
        />
      </AbsoluteFill>
      <AbsoluteFill
        style={{
          top: 340,
          height: 760,
          left: 44,
          right: 44,
          borderRadius: 28,
          overflow: 'hidden',
          border: '2px solid rgba(244,239,228,0.22)',
          boxShadow: '0 30px 80px rgba(0,0,0,0.48)',
          background: '#0d0d0d',
        }}
      >
        <Video
          src={staticFile('hai-promo.mp4')}
          objectFit="contain"
          style={{width: '100%', height: '100%'}}
        />
      </AbsoluteFill>
    </>
  );
};

export const HaiVerticalPromo: React.FC = () => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  const progress = interpolate(frame, [0, 40.8 * fps], [0, 1], clamp);
  const topLogoOpacity = interpolate(frame, [0, 0.5 * fps, 38 * fps, 40 * fps], [0, 1, 1, 0], clamp);

  return (
    <AbsoluteFill style={{backgroundColor: INK, fontFamily: 'Arial, PingFang SC, sans-serif'}}>
      <SourceVideo />

      <AbsoluteFill
        style={{
          background:
            'linear-gradient(180deg, rgba(12,11,10,0.85) 0%, rgba(12,11,10,0.05) 24%, rgba(12,11,10,0.12) 55%, rgba(12,11,10,0.92) 100%)',
        }}
      />

      <div
        style={{
          position: 'absolute',
          top: 86,
          left: 72,
          color: CREAM,
          fontSize: 34,
          fontWeight: 900,
          letterSpacing: 5,
          opacity: topLogoOpacity,
        }}
      >
        HAI
      </div>
      <div
        style={{
          position: 'absolute',
          top: 92,
          right: 72,
          color: '#bcb4a3',
          fontSize: 24,
          fontWeight: 700,
          letterSpacing: 1.5,
          opacity: topLogoOpacity,
        }}
      >
        v0.1.2
      </div>
      <div
        style={{
          position: 'absolute',
          left: 72,
          right: 72,
          top: 245,
          height: 4,
          borderRadius: 4,
          background: 'rgba(244,239,228,0.3)',
          overflow: 'hidden',
        }}
      >
        <div style={{width: `${progress * 100}%`, height: '100%', background: RED}} />
      </div>

      <Pill top={1150}>不是聊天框，是 AI Agent 工作台</Pill>

      <Sequence from={0} durationInFrames={4.4 * fps}>
        <Caption start={0.2} end={4.2}>
          AI 最怕的不是不会写代码
          <br />
          而是做到一半忘了自己在干嘛
        </Caption>
      </Sequence>
      <Sequence from={4.2 * fps} durationInFrames={6.2 * fps}>
        <Caption start={4.5} end={10.1}>
          普通 AI 往往只给你一段答案
          <br />
          真正做项目，需要它自己动手
        </Caption>
      </Sequence>
      <Sequence from={10.2 * fps} durationInFrames={7.2 * fps}>
        <Caption start={10.5} end={17.1}>
          读取文件 · 调用工具 · 修改代码
          <br />
          每一步都有记录
        </Caption>
      </Sequence>
      <Sequence from={17.2 * fps} durationInFrames={7.6 * fps}>
        <Caption start={17.5} end={24.5}>
          模型调用、工具执行、文件 Diff
          <br />
          到底做了什么，打开 Trace 就知道
        </Caption>
      </Sequence>
      <Sequence from={24.6 * fps} durationInFrames={7.2 * fps}>
        <Caption start={24.9} end={31.5}>
          可以持续运行，也可以中途恢复
          <br />
          这才像真正的项目助手
        </Caption>
      </Sequence>
      <Sequence from={31.6 * fps} durationInFrames={8.8 * fps}>
        <Caption start={32} end={39.8} small>
          HAI v0.1.2 开源发布
          <br />
          github.com/seven7628/hai-harness
        </Caption>
      </Sequence>

      <div
        style={{
          position: 'absolute',
          left: 72,
          right: 72,
          bottom: 108,
          display: 'flex',
          justifyContent: 'space-between',
          alignItems: 'center',
          color: '#bcb4a3',
          fontSize: 24,
          fontWeight: 700,
        }}
      >
        <span>本地优先 · 可追踪 · 可恢复</span>
        <span>Apple Silicon</span>
      </div>
    </AbsoluteFill>
  );
};
