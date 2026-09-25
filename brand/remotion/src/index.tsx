import React from 'react';
import {Composition, registerRoot} from 'remotion';
import {HaiVerticalPromo} from './HaiVerticalPromo';

export const RemotionRoot: React.FC = () => {
  return (
    <Composition
      id="HAI-v012-vertical"
      component={HaiVerticalPromo}
      durationInFrames={1224}
      fps={30}
      width={1080}
      height={1920}
    />
  );
};

registerRoot(RemotionRoot);
