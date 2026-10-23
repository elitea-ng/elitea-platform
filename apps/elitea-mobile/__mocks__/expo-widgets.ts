// jest: expo-widgets has no native module under jest. Widgets and Live Activities record what
// the app wrote; the 'widget' layouts arrive as their serialised source (babel-preset-expo's
// widgets plugin), which src/test/widgetRuntime.ts evaluates against stub SwiftUI views.

export const layouts = new Map<string, unknown>();

export const createWidget = jest.fn((name: string, layout: unknown, initialProps?: object) => {
  layouts.set(name, layout);
  return {
    name,
    initialProps,
    updateTimeline: jest.fn(),
    updateSnapshot: jest.fn(),
    reload: jest.fn(),
    getTimeline: jest.fn(async () => []),
    setConfigurationParameterEnum: jest.fn(),
  };
});

export interface FakeActivity {
  id: string;
  props: object;
  url?: string;
  ended: boolean;
  getId(): string;
  update: jest.Mock;
  end: jest.Mock;
  getPushToken: jest.Mock;
  addPushTokenListener: jest.Mock;
}

let nextActivity = 1;
export const activities: FakeActivity[] = [];

export const createLiveActivity = jest.fn((name: string, layout: unknown) => {
  layouts.set(name, layout);
  return {
    name,
    start: jest.fn((props: object, url?: string) => {
      const activity: FakeActivity = {
        id: `activity-${nextActivity++}`,
        props,
        url,
        ended: false,
        getId() {
          return this.id;
        },
        update: jest.fn(async function (this: FakeActivity, next: object) {
          this.props = next;
        }),
        end: jest.fn(async function (this: FakeActivity, _policy?: unknown, next?: object) {
          this.ended = true;
          if (next) this.props = next;
        }),
        getPushToken: jest.fn(async () => null),
        addPushTokenListener: jest.fn(() => ({ remove: jest.fn() })),
      };
      activities.push(activity);
      return activity;
    }),
    getInstances: jest.fn(() => activities.filter((a) => !a.ended)),
  };
});

export const after = (date: Date) => ({ after: date });
export const addUserInteractionListener = jest.fn(() => ({ remove: jest.fn() }));
export const addPushToStartTokenListener = jest.fn(() => ({ remove: jest.fn() }));
export const widgetsDirectory = 'file:///widgets/';
