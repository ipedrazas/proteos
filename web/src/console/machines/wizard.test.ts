import { describe, expect, it } from 'vitest';
import type { MachineTemplate } from '../../api/client';
import { clamp, clampResources, defaultResources, WIZARD_STEPS } from './wizard';

function template(over: Partial<MachineTemplate> = {}): MachineTemplate {
  return {
    id: 'go',
    label: 'Go',
    description: 'Go toolchain',
    defaults: { vcpus: 2, mem_mib: 4096, disk_mib: 10240 },
    limits: {
      vcpus: { min: 1, max: 8 },
      mem_mib: { min: 1024, max: 16384 },
      disk_mib: { min: 5120, max: 51200 },
    },
    ...over,
  };
}

describe('clamp', () => {
  it('leaves a value inside the range alone', () => {
    expect(clamp(4, 1, 8)).toBe(4);
  });

  it('pins to the bounds', () => {
    expect(clamp(0, 1, 8)).toBe(1);
    expect(clamp(99, 1, 8)).toBe(8);
  });

  it('accepts the bounds themselves', () => {
    expect(clamp(1, 1, 8)).toBe(1);
    expect(clamp(8, 1, 8)).toBe(8);
  });

  it('rounds to an integer', () => {
    expect(clamp(2.4, 1, 8)).toBe(2);
    expect(clamp(2.6, 1, 8)).toBe(3);
  });

  it('collapses NaN to the minimum rather than propagating it', () => {
    // An emptied number field yields NaN, which fails every comparison
    // downstream and would reach the API untouched.
    expect(clamp(Number.NaN, 1, 8)).toBe(1);
    expect(clamp(Number.POSITIVE_INFINITY, 1, 8)).toBe(8);
    expect(clamp(Number.NEGATIVE_INFINITY, 1, 8)).toBe(1);
  });
});

describe('clampResources', () => {
  it('passes a spec that is already within limits', () => {
    const r = { vcpus: 2, mem_mib: 4096, disk_mib: 10240 };
    expect(clampResources(r, template())).toEqual(r);
  });

  it('pins every dimension independently', () => {
    expect(clampResources({ vcpus: 99, mem_mib: 1, disk_mib: 99999 }, template())).toEqual({
      vcpus: 8,
      mem_mib: 1024,
      disk_mib: 51200,
    });
  });

  it('brings a spec from a wider template into a narrower one', () => {
    // Switching template must not leave the previous template's values behind.
    const wide = { vcpus: 8, mem_mib: 16384, disk_mib: 51200 };
    const narrow = template({
      limits: {
        vcpus: { min: 1, max: 2 },
        mem_mib: { min: 1024, max: 2048 },
        disk_mib: { min: 5120, max: 10240 },
      },
    });
    expect(clampResources(wide, narrow)).toEqual({
      vcpus: 2,
      mem_mib: 2048,
      disk_mib: 10240,
    });
  });
});

describe('defaultResources', () => {
  it('uses the template defaults when they are inside its limits', () => {
    expect(defaultResources(template())).toEqual({ vcpus: 2, mem_mib: 4096, disk_mib: 10240 });
  });

  it('absorbs a catalog whose defaults sit outside its own limits', () => {
    // The catalog is operator-authored config; a typo there is a deployment
    // problem, not something to forward to the API as invalid_resources.
    const broken = template({ defaults: { vcpus: 99, mem_mib: 1, disk_mib: 10240 } });
    expect(defaultResources(broken)).toEqual({ vcpus: 8, mem_mib: 1024, disk_mib: 10240 });
  });
});

describe('WIZARD_STEPS', () => {
  it('runs name → template → size → review', () => {
    expect(WIZARD_STEPS.map((s) => s.key)).toEqual(['name', 'template', 'size', 'review']);
  });
});
